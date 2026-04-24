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

// retryableError 标记可重试的非网络错误（如 HTTP 5xx）。
type retryableError struct{ error }

// doWithRetry 对网络错误和 retryableError 最多重试 maxRetries 次，每次间隔 retryDelay。
func doWithRetry(maxRetries int, retryDelay time.Duration, fn func() error) error {
	var err error
	for i := range maxRetries + 1 {
		err = fn()
		if err == nil {
			return nil
		}
		var netErr interface{ Timeout() bool }
		var retryErr retryableError
		canRetry := errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.As(err, &retryErr)
		if !canRetry {
			return err
		}
		if i < maxRetries {
			log.Printf("api: request failed (attempt %d/%d): %v, retrying in %s", i+1, maxRetries+1, err, retryDelay)
			time.Sleep(retryDelay)
		}
	}
	return err
}

// checkHTTPStatus 检查 HTTP 响应状态码，5xx 返回可重试错误，其他非 2xx 返回不可重试错误。
func checkHTTPStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	err := fmt.Errorf("http %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	if resp.StatusCode >= 500 {
		return retryableError{err}
	}
	return err
}

// userDTO 同步用户数据。Tier 为空时表示后端老格式或用户未分级（兼容模式）。
type userDTO struct {
	UUID string `json:"uuid"`
	Tier string `json:"tier,omitempty"`
}

// 老格式（兼容）：data 是 userDTO 数组
type apiResp struct {
	Code int       `json:"code"`
	Data []userDTO `json:"data"`
}

// 新格式 V2：data 是对象，含 tiers 字典和 users 数组
type tierDTO struct {
	MarkID     int    `json:"markId"`
	InboundTag string `json:"inboundTag"`
	PoolMbps   int    `json:"poolMbps"`
}

type apiRespV2Data struct {
	Tiers map[string]tierDTO `json:"tiers"`
	Users []userDTO          `json:"users"`
}

type apiRespV2 struct {
	Code int           `json:"code"`
	Data apiRespV2Data `json:"data"`
}

type deltaResp struct {
	Code int       `json:"code"`
	Data deltaData `json:"data"`
}

// deltaData 兼容新老格式：老格式 Added 是 []string，新格式是 []userDTO。
// 统一用 []userDTO 承载，老格式解析时 Tier 字段为空。
type deltaData struct {
	Added   []userDTO `json:"-"`
	Removed []string  `json:"removed"`
}

// rawDeltaData 用于两次解析：先试新格式（Added 是对象数组），失败再试老格式（Added 是字符串数组）。
type rawDeltaData struct {
	Added   json.RawMessage `json:"added"`
	Removed []string        `json:"removed"`
}

type deltaRespRaw struct {
	Code int          `json:"code"`
	Data rawDeltaData `json:"data"`
}

// setAuthHeaders 给请求加上认证 + agent 版本 header。
func (s *XrayUserSync) setAuthHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("X-Agent-Version", "2")
}

// fetchUsers 从服务端拉取全量有效用户列表，同时更新 tiers 缓存。
// 后端按 X-Agent-Version: 2 header 返回新/老格式。
func (s *XrayUserSync) fetchUsers() ([]userDTO, error) {
	var result []userDTO
	err := doWithRetry(2, apiRetryDelay, func() error {
		req, err := http.NewRequest(http.MethodGet, s.apiBase+"/api/agent/xray/users", nil)
		if err != nil {
			return err
		}
		s.setAuthHeaders(req)

		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if err := checkHTTPStatus(resp); err != nil {
			return fmt.Errorf("fetch users: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		// 先尝试新格式（data 是对象）
		var v2 apiRespV2
		if err := json.Unmarshal(body, &v2); err == nil && v2.Code == 200 {
			// 判定是否真的是新格式：data 对象里至少有 tiers 或 users 任一字段
			if v2.Data.Tiers != nil || v2.Data.Users != nil {
				tiers := make(map[string]TierConfig, len(v2.Data.Tiers))
				for name, t := range v2.Data.Tiers {
					tiers[name] = TierConfig{MarkID: t.MarkID, InboundTag: t.InboundTag, PoolMbps: t.PoolMbps}
				}
				s.mu.Lock()
				s.tiers = tiers
				// s.defaultTier 只在迁移指令时才被显式设置。重启后丢失，这里补一个推断：
				// 优先 "vip"（全局约定），否则挑任一 tier。保证空 tier 的 AddUser 能落到
				// 一个真实存在的 inbound 上。
				if s.defaultTier == "" && len(tiers) > 0 {
					if _, ok := tiers["vip"]; ok {
						s.defaultTier = "vip"
					} else {
						for name := range tiers {
							s.defaultTier = name
							break
						}
					}
				}
				s.mu.Unlock()
				if v2.Data.Users != nil {
					result = v2.Data.Users
				} else {
					result = []userDTO{}
				}
				return nil
			}
		}

		// 老格式 fallback（data 是数组）
		var v1 apiResp
		if err := json.Unmarshal(body, &v1); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
		if v1.Code != 200 {
			return fmt.Errorf("api returned code=%d", v1.Code)
		}
		// 老格式：清空 tiers 进入兼容模式
		s.mu.Lock()
		s.tiers = map[string]TierConfig{}
		s.mu.Unlock()
		result = v1.Data
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

		if err := checkHTTPStatus(resp); err != nil {
			return fmt.Errorf("fetch temp users: %w", err)
		}

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

// fetchDelta 从服务端拉取增量变更。返回统一结构（Added 是 []userDTO）。
func (s *XrayUserSync) fetchDelta(since int64) (*deltaData, error) {
	var result *deltaData
	err := doWithRetry(2, apiRetryDelay, func() error {
		url := fmt.Sprintf("%s/api/agent/xray/users/delta?since=%d", s.apiBase, since)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		s.setAuthHeaders(req)

		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if err := checkHTTPStatus(resp); err != nil {
			return fmt.Errorf("fetch delta: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		var raw deltaRespRaw
		if err := json.Unmarshal(body, &raw); err != nil {
			return fmt.Errorf("parse delta response: %w", err)
		}
		if raw.Code != 200 {
			return fmt.Errorf("delta api returned code=%d", raw.Code)
		}

		data := &deltaData{Removed: raw.Data.Removed}

		// Added 可能是 []userDTO（新格式）或 []string（老格式）
		if len(raw.Data.Added) > 0 {
			var asUsers []userDTO
			if err := json.Unmarshal(raw.Data.Added, &asUsers); err == nil && len(asUsers) > 0 && asUsers[0].UUID != "" {
				data.Added = asUsers
			} else {
				var asStrings []string
				if err := json.Unmarshal(raw.Data.Added, &asStrings); err != nil {
					return fmt.Errorf("parse delta added (neither []userDTO nor []string): %w", err)
				}
				data.Added = make([]userDTO, len(asStrings))
				for i, uuid := range asStrings {
					data.Added[i] = userDTO{UUID: uuid}
				}
			}
		} else {
			data.Added = []userDTO{}
		}
		result = data
		return nil
	})
	return result, err
}
