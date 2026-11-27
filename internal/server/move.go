// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mattmezza/mimux/internal/store"
)

// The local placement changes immediately. Waiting to send the IMAP move
// gives the user time to undo without changing the message's server UID.
const undoGrace = 11 * time.Second

type pendingMove struct {
	store.MessageMove
	timer     *time.Timer
	undoToken string
}

type threadMoveResult struct {
	IDs       []string `json:"ids"`
	UndoToken string   `json:"undo_token,omitempty"`
}

func (s *Server) handleMove(target string) http.HandlerFunc {
	return s.handleSpecialMove(target, false)
}

func (s *Server) handleThreadMove(target string) http.HandlerFunc {
	return s.handleSpecialMove(target, true)
}

func (s *Server) handleSpecialMove(target string, wholeThread bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.pendingMu.Lock()
		defer s.pendingMu.Unlock()
		msg := s.messageFromReq(w, r)
		if msg == nil {
			return
		}
		msgs := []store.Message{*msg}
		if wholeThread {
			if thread := s.conversationOf(msg.ID); thread != nil {
				msgs = thread.Messages
			}
		}
		targets := map[string]*store.Folder{}
		moves := make([]store.MessageMove, 0, len(msgs))
		for _, msg := range msgs {
			tf := targets[msg.Account]
			if tf == nil {
				var err error
				tf, err = s.store.FolderBySpecial(msg.Account, target)
				if err != nil || tf == nil {
					http.Error(w, "no "+target+" folder for "+msg.Account, http.StatusBadRequest)
					return
				}
				targets[msg.Account] = tf
			}
			if !appendMove(w, &moves, &msg, tf.ID) {
				return
			}
		}
		var undoToken string
		if wholeThread && len(moves) > 0 {
			undoToken = rand.Text()
		}
		if err := s.queueMoves(moves, undoToken); err != nil {
			writeMoveError(w, err)
			return
		}
		if wholeThread {
			ids := make([]string, 0, len(moves))
			for _, move := range moves {
				ids = append(ids, strconv.FormatInt(move.ID, 10))
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(threadMoveResult{IDs: ids, UndoToken: undoToken})
		} else if len(moves) == 0 {
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func (s *Server) handleMoveToFolder(w http.ResponseWriter, r *http.Request) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	msg := s.messageFromReq(w, r)
	if msg == nil {
		return
	}
	fid, err := strconv.ParseInt(r.FormValue("folder"), 10, 64)
	if err != nil {
		http.Error(w, "missing folder", http.StatusBadRequest)
		return
	}
	tf, err := s.store.FolderByID(fid)
	if err != nil || tf == nil {
		http.Error(w, "no such folder", http.StatusBadRequest)
		return
	}
	if tf.Account != msg.Account {
		http.Error(w, "folder belongs to a different account", http.StatusBadRequest)
		return
	}
	var moves []store.MessageMove
	if !appendMove(w, &moves, msg, tf.ID) {
		return
	}
	if len(moves) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.queueMoves(moves, ""); err != nil {
		writeMoveError(w, err)
	}
}

// Requests for the visible destination are idempotent, including during undo.
func appendMove(w http.ResponseWriter, moves *[]store.MessageMove, msg *store.Message, targetID int64) bool {
	if msg.FolderID == targetID {
		return true
	}
	if msg.SourceFolderID != 0 {
		writeMoveError(w, store.ErrMessageMoveChanged)
		return false
	}
	*moves = append(*moves, store.MessageMove{ID: msg.ID, SourceID: msg.FolderID, TargetID: targetID})
	return true
}

func writeMoveError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrMessageMoveChanged) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Error(w, "Couldn't update the message location.", http.StatusInternalServerError)
}

// queueMoves runs with pendingMu held, so another request sees both the new
// placement and its original undo timer, or neither.
func (s *Server) queueMoves(moves []store.MessageMove, undoToken string) error {
	if err := s.store.StageMessageMoves(moves); err != nil {
		return err
	}
	for _, move := range moves {
		pending := &pendingMove{MessageMove: move, undoToken: undoToken}
		s.pending[move.ID] = pending
		pending.timer = time.AfterFunc(undoGrace, func() {
			s.pendingMu.Lock()
			active := s.pending[move.ID] == pending
			if active {
				delete(s.pending, move.ID)
			}
			s.pendingMu.Unlock()
			if active {
				s.background(func(ctx context.Context) error { return s.mail.MovePending(ctx, move) })
			}
		})
	}
	s.recountMoveFolders(moves)
	return nil
}

func (s *Server) handleUndoMove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	folderID, err := strconv.ParseInt(r.FormValue("folder"), 10, 64)
	if err != nil || folderID <= 0 {
		http.Error(w, "missing folder", http.StatusBadRequest)
		return
	}
	s.undoMoves(w, []int64{id}, folderID, "")
}

func (s *Server) handleUndoMoves(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || len(r.PostForm["id"]) == 0 {
		http.Error(w, "missing messages", http.StatusBadRequest)
		return
	}
	ids := make([]int64, 0, len(r.PostForm["id"]))
	for _, value := range r.PostForm["id"] {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			http.Error(w, "invalid message id", http.StatusBadRequest)
			return
		}
		ids = append(ids, id)
	}
	token := r.PostForm.Get("undo_token")
	if token == "" {
		http.Error(w, "missing undo token", http.StatusBadRequest)
		return
	}
	s.undoMoves(w, ids, 0, token)
}

func (s *Server) undoMoves(w http.ResponseWriter, ids []int64, folderID int64, token string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	moves := make([]store.MessageMove, 0, len(ids))
	for _, id := range ids {
		pending := s.pending[id]
		if pending == nil || pending.undoToken != token || (folderID != 0 && pending.SourceID != folderID) {
			http.Error(w, "too late to undo", http.StatusConflict)
			return
		}
		moves = append(moves, pending.MessageMove)
	}
	if err := s.store.CancelMessageMoves(moves); err != nil {
		writeMoveError(w, err)
		return
	}
	for _, move := range moves {
		s.pending[move.ID].timer.Stop()
		delete(s.pending, move.ID)
	}
	s.recountMoveFolders(moves)
}

func (s *Server) recountMoveFolders(moves []store.MessageMove) {
	folders := map[int64]bool{}
	for _, move := range moves {
		folders[move.SourceID] = true
		folders[move.TargetID] = true
	}
	for id := range folders {
		_ = s.store.RecountUnread(id)
	}
}
