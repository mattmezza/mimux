package store

import (
	"database/sql"
	"strconv"
)

// SummaryCacheKey keys a cached AI summary on the message and its detail level,
// so switching level re-bills once and never again.
// NOTE: no body hash — a message's body is immutable; a re-fetch that really
// changed it would keep the old summary until the row is deleted.
func SummaryCacheKey(messageID int64, level string) string {
	return strconv.FormatInt(messageID, 10) + ":" + level
}

// SummaryCached returns a cached summary, ok=false on miss.
func (s *Store) SummaryCached(key string) (summary string, truncated, ok bool, err error) {
	err = s.DB.QueryRow(`SELECT summary, truncated FROM summaries WHERE cache_key = ?`, key).
		Scan(&summary, &truncated)
	if err == sql.ErrNoRows {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	return summary, truncated, true, nil
}

// SaveSummary upserts a cache entry.
func (s *Store) SaveSummary(key, summary string, truncated bool) error {
	_, err := s.DB.Exec(`INSERT INTO summaries (cache_key, summary, truncated) VALUES (?, ?, ?)
		ON CONFLICT(cache_key) DO UPDATE SET summary = excluded.summary, truncated = excluded.truncated`,
		key, summary, truncated)
	return err
}
