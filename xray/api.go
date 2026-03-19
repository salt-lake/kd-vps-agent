//go:build xray

package xray

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

type userDTO struct {
	UUID string `json:"uuid"`
}

type apiResp struct {
	Code int       `json:"code"`
	Data []userDTO `json:"data"`
}

type deltaResp struct {
	Code int       `json:"code"`
	Data deltaData `json:"data"`
}

type deltaData struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
}

// fetchUsers 从服务端拉取全量有效用户列表。
func (s *XrayUserSync) fetchUsers() ([]userDTO, error) {
	req, err := http.NewRequest(http.MethodGet, s.apiBase+"/api/agent/xray/users", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var ar apiResp
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if ar.Code != 200 {
		return nil, fmt.Errorf("api returned code=%d", ar.Code)
	}
	return ar.Data, nil
}

type tempUsersResp struct {
	Code int           `json:"code"`
	Data tempUsersData `json:"data"`
}

type tempUsersData struct {
	Version string   `json:"version"`
	UUIDs   []string `json:"uuids"`
}

// fetchTempUsers 拉取临时用户列表。
func fetchTempUsers(apiBase, token string) (version string, uuids []string, err error) {
	req, err := http.NewRequest(http.MethodGet, apiBase+"/api/nodes/actions/temp-users", nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}

	var tr tempUsersResp
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", nil, fmt.Errorf("parse temp users response: %w", err)
	}
	if tr.Code != 200 {
		return "", nil, fmt.Errorf("temp users api returned code=%d", tr.Code)
	}
	return tr.Data.Version, tr.Data.UUIDs, nil
}

// fetchDelta 从服务端拉取增量变更。
func (s *XrayUserSync) fetchDelta(since int64) (*deltaData, error) {
	url := fmt.Sprintf("%s/api/agent/xray/users/delta?since=%d", s.apiBase, since)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var dr deltaResp
	if err := json.Unmarshal(body, &dr); err != nil {
		return nil, fmt.Errorf("parse delta response: %w", err)
	}
	if dr.Code != 200 {
		return nil, fmt.Errorf("delta api returned code=%d", dr.Code)
	}
	return &dr.Data, nil
}
