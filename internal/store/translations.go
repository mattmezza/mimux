package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
)

// TranslationCacheKey hashes source text + target language into a stable key.
func TranslationCacheKey(text, target string) string {
	sum := sha256.Sum256([]byte(target + "\x00" + text))
	return hex.EncodeToString(sum[:])
}

// TranslationCached returns a cached translation, ok=false on miss.
func (s *Store) TranslationCached(key string) (translated, detectedLang string, ok bool, err error) {
	err = s.DB.QueryRow(`SELECT translated_text, detected_lang FROM translations WHERE cache_key = ?`, key).
		Scan(&translated, &detectedLang)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return translated, detectedLang, true, nil
}

// SaveTranslation upserts a cache entry.
func (s *Store) SaveTranslation(key, translated, detectedLang string) error {
	_, err := s.DB.Exec(`INSERT INTO translations (cache_key, translated_text, detected_lang) VALUES (?, ?, ?)
		ON CONFLICT(cache_key) DO UPDATE SET translated_text = excluded.translated_text, detected_lang = excluded.detected_lang`,
		key, translated, detectedLang)
	return err
}
