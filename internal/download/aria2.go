package download

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Aria2Client communicates with aria2c via JSON-RPC.
type Aria2Client struct {
	url    string
	client *http.Client
}

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      string        `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Aria2Status holds download status from aria2c.
type Aria2Status struct {
	GID             string `json:"gid"`
	Status          string `json:"status"`          // active, waiting, paused, error, complete, removed
	TotalLength     int64  `json:"totalLength"`
	CompletedLength int64  `json:"completedLength"`
	DownloadSpeed   int64  `json:"downloadSpeed"`
	ErrorCode       string `json:"errorCode"`
	ErrorMessage    string `json:"errorMessage"`
	Dir             string `json:"dir"`
	Files           []struct {
		Path string `json:"path"`
		URIs []struct {
			URI string `json:"uri"`
		} `json:"uris"`
	} `json:"files"`
}

func (s *Aria2Status) UnmarshalJSON(data []byte) error {
	// aria2c returns numbers as strings in JSON-RPC
	type raw struct {
		GID             string `json:"gid"`
		Status          string `json:"status"`
		TotalLength     string `json:"totalLength"`
		CompletedLength string `json:"completedLength"`
		DownloadSpeed   string `json:"downloadSpeed"`
		ErrorCode       string `json:"errorCode"`
		ErrorMessage    string `json:"errorMessage"`
		Dir             string `json:"dir"`
		Files           []struct {
			Path string `json:"path"`
			URIs []struct {
				URI string `json:"uri"`
			} `json:"uris"`
		} `json:"files"`
	}

	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}

	s.GID = r.GID
	s.Status = r.Status
	s.ErrorCode = r.ErrorCode
	s.ErrorMessage = r.ErrorMessage
	s.Dir = r.Dir
	s.Files = r.Files
	fmt.Sscanf(r.TotalLength, "%d", &s.TotalLength)
	fmt.Sscanf(r.CompletedLength, "%d", &s.CompletedLength)
	fmt.Sscanf(r.DownloadSpeed, "%d", &s.DownloadSpeed)

	return nil
}

func NewAria2Client(port int) *Aria2Client {
	return &Aria2Client{
		url: fmt.Sprintf("http://127.0.0.1:%d/jsonrpc", port),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (c *Aria2Client) call(method string, params ...interface{}) (json.RawMessage, error) {
	if params == nil {
		params = []interface{}{}
	}

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      "aria-tui",
		Method:  method,
		Params:  params,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	resp, err := c.client.Post(c.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var rpcResp rpcResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("aria2 error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return rpcResp.Result, nil
}

// AddURI adds a download URL and returns the GID.
func (c *Aria2Client) AddURI(url string) (string, error) {
	result, err := c.call("aria2.addUri", []string{url})
	if err != nil {
		return "", err
	}

	var gid string
	if err := json.Unmarshal(result, &gid); err != nil {
		return "", err
	}
	return gid, nil
}

// TellStatus returns the status of a download by GID.
func (c *Aria2Client) TellStatus(gid string) (*Aria2Status, error) {
	result, err := c.call("aria2.tellStatus", gid)
	if err != nil {
		return nil, err
	}

	var status Aria2Status
	if err := json.Unmarshal(result, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// Pause pauses a download.
func (c *Aria2Client) Pause(gid string) error {
	_, err := c.call("aria2.pause", gid)
	return err
}

// Unpause resumes a paused download.
func (c *Aria2Client) Unpause(gid string) error {
	_, err := c.call("aria2.unpause", gid)
	return err
}

// Remove removes a download.
func (c *Aria2Client) Remove(gid string) error {
	_, err := c.call("aria2.remove", gid)
	return err
}

// GetVersion checks if aria2c is responsive.
func (c *Aria2Client) GetVersion() (string, error) {
	result, err := c.call("aria2.getVersion")
	if err != nil {
		return "", err
	}

	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(result, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}
