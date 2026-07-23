package store

import "database/sql"

// RSVP is the user's recorded response to a calendar invite (see migration
// 0090).
type RSVP struct {
	PartStat string // ACCEPTED | TENTATIVE | DECLINED
	Sequence int    // event SEQUENCE we replied to
}

// SaveRSVP records (or overwrites) the user's response to invite uid on
// account, for the given event sequence.
func (s *Store) SaveRSVP(account, uid, partstat string, sequence int) error {
	_, err := s.DB.Exec(`
		INSERT INTO calendar_rsvps (account, uid, partstat, sequence, updated_at)
		VALUES (?, ?, ?, ?, datetime('now'))
		ON CONFLICT(account, uid) DO UPDATE SET
			partstat = excluded.partstat, sequence = excluded.sequence, updated_at = excluded.updated_at`,
		account, uid, partstat, sequence)
	return err
}

// GetRSVP returns the recorded response for invite uid on account, if any.
func (s *Store) GetRSVP(account, uid string) (RSVP, bool, error) {
	var r RSVP
	err := s.DB.QueryRow(`SELECT partstat, sequence FROM calendar_rsvps WHERE account = ? AND uid = ?`,
		account, uid).Scan(&r.PartStat, &r.Sequence)
	if err == sql.ErrNoRows {
		return RSVP{}, false, nil
	}
	if err != nil {
		return RSVP{}, false, err
	}
	return r, true, nil
}
