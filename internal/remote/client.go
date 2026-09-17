// Package remote shares a cache between machines.
//
// The whole design rests on one property established in Milestone 4: a cache
// key contains no absolute paths and no modification times. Two checkouts of
// the same sources, on two machines, at two different paths, compute the same
// key. Without that, a shared cache would never hit and this package would have
// nothing to do.
package remote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mghadia1/buildforge/internal/cache"
	"github.com/mghadia1/buildforge/internal/digest"
)

// maxBlobBytes bounds what a client will accept from a server. A cache is a
// thing other machines write to, so a response with no limit is a way for one
// of them to exhaust this machine's memory.
const maxBlobBytes = 256 << 20 // 256 MiB

// Client talks to a BuildForge cache server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a client for a cache server.
func NewClient(baseURL string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("remote cache URL must be http or https, got %q", baseURL)
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		// A build must not hang because a cache server is wedged. Every request
		// gives up quickly and the caller falls back to running the action.
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// GetBlob fetches a blob, verifying it against the digest that was asked for.
//
// Verification matters most here. These bytes came from another machine over a
// network; trusting them would mean a damaged or hostile server could install
// arbitrary content as the output of a build action.
func (c *Client) GetBlob(d digest.Digest) ([]byte, error) {
	b, found, err := c.get("/cas/" + string(d))
	if err != nil || !found {
		return nil, err
	}
	if got := digest.Bytes(b); got != d {
		return nil, fmt.Errorf("%w: server sent %s for %s", cache.ErrCorrupt, got, d)
	}
	return b, nil
}

// PutBlob uploads a blob.
func (c *Client) PutBlob(d digest.Digest, b []byte) error {
	return c.put("/cas/"+string(d), b)
}

// GetEntry fetches an action cache entry, or nil when the server does not have
// one.
func (c *Client) GetEntry(key string) (*cache.Entry, error) {
	b, found, err := c.get("/ac/" + key)
	if err != nil || !found {
		return nil, err
	}
	var e cache.Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	if e.Key != key {
		return nil, fmt.Errorf("server returned an entry for %q under key %q", e.Key, key)
	}
	return &e, nil
}

// PutEntry uploads an action cache entry.
func (c *Client) PutEntry(e *cache.Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.put("/ac/"+e.Key, b)
}

func (c *Client) get(path string) ([]byte, bool, error) {
	resp, err := c.HTTP.Get(c.BaseURL + path)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("GET %s: %s", path, resp.Status)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBlobBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > maxBlobBytes {
		return nil, false, fmt.Errorf("GET %s: response exceeds %d bytes", path, maxBlobBytes)
	}
	return b, true, nil
}

func (c *Client) put(path string, body []byte) error {
	req, err := http.NewRequest(http.MethodPut, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("PUT %s: %s", path, resp.Status)
	}
	return nil
}
