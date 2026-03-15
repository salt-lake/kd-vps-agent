//go:build xray

package xray

import (
	"encoding/json"
	"os"
)

const syncStateFile = "/var/lib/node-agent/sync_state.json"

type syncState struct {
	LastSyncTime int64 `json:"last_sync_time"` // unix seconds
}

func loadSyncState() (syncState, error) {
	data, err := os.ReadFile(syncStateFile)
	if err != nil {
		return syncState{}, err
	}
	var s syncState
	if err := json.Unmarshal(data, &s); err != nil {
		return syncState{}, err
	}
	return s, nil
}

func saveSyncState(s syncState) error {
	if err := os.MkdirAll("/var/lib/node-agent", 0755); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(syncStateFile, data, 0644)
}
