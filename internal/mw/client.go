package mw

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (c *Client) Fetch(ctx context.Context, path string, rawQuery string, headers http.Header) (*http.Response, []byte, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, nil, err
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = rawQuery

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	copyHeaders(req.Header, headers)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if len(vv) == 0 {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
