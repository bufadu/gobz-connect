package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
)

// StoredSecrets holds the Qobuz app credentials persisted to disk.
type StoredSecrets struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

// loadSecrets reads the secrets file. Returns nil (no error) if the file does not exist.
func loadSecrets(path string) (*StoredSecrets, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s StoredSecrets
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// saveSecrets writes the secrets to the given file path (creates or overwrites).
func saveSecrets(path string, s *StoredSecrets) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	log.Printf("secrets: saved app credentials to %s", path)
	return nil
}
