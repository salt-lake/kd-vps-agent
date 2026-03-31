//go:build xray

package xray

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}
var apiRetryDelay = 5 * time.Second

// doWithRetry 对网络错误最多重试 maxRetries 次，每次间隔 retryDelay。
// 业务错误（HTTP 状态码异常、JSON 解析失败）不重试。
func doWithRetry(maxRetries int, retryDelay time.Duration, fn func() error) error {
	var err error
	for i := range maxRetries + 1 {
		err = fn()
		if err == nil {
			return nil
		}
		var netErr interface{ Timeout() bool }
		isNetwork := errors.As(err, &netErr) || errors.Is(err, io.EOF)
		if !isNetwork {
			return err
		}
		if i < maxRetries {
			log.Printf("api: request failed (attempt %d/%d): %v, retrying in %s", i+1, maxRetries+1, err, retryDelay)
			time.Sleep(retryDelay)
		}
	}
	return err
}

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
	var result []userDTO
	err := doWithRetry(2, apiRetryDelay, func() error {
		req, err := http.NewRequest(http.MethodGet, s.apiBase+"/api/agent/xray/users", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+s.token)

		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		var ar apiResp
		if err := json.Unmarshal(body, &ar); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
		if ar.Code != 200 {
			return fmt.Errorf("api returned code=%d", ar.Code)
		}
		result = ar.Data
		return nil
	})
	return result, err
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
	var v string
	var u []string
	err = doWithRetry(2, apiRetryDelay, func() error {
		req, err := http.NewRequest(http.MethodGet, apiBase+"/api/agent/temp-users", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		var tr tempUsersResp
		if err := json.Unmarshal(body, &tr); err != nil {
			return fmt.Errorf("parse temp users response: %w", err)
		}
		if tr.Code != 200 {
			return fmt.Errorf("temp users api returned code=%d", tr.Code)
		}
		v, u = tr.Data.Version, tr.Data.UUIDs
		return nil
	})
	return v, u, err
}

// fetchDelta 从服务端拉取增量变更。
func (s *XrayUserSync) fetchDelta(since int64) (*deltaData, error) {
	var result *deltaData
	err := doWithRetry(2, apiRetryDelay, func() error {
		url := fmt.Sprintf("%s/api/agent/xray/users/delta?since=%d", s.apiBase, since)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+s.token)

		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		var dr deltaResp
		if err := json.Unmarshal(body, &dr); err != nil {
			return fmt.Errorf("parse delta response: %w", err)
		}
		if dr.Code != 200 {
			return fmt.Errorf("delta api returned code=%d", dr.Code)
		}
		result = &dr.Data
		return nil
	})
	return result, err
}
