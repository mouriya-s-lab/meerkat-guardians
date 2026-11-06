// Package github reads issues and issue comments through the GitHub REST API. The program only
// reads GitHub; the plugin's tools write it (C4: no arrow from the program to the repositories).
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// Client is the shared REST client, authenticated with the token from GH_TOKEN.
type Client struct {
	rest *api.RESTClient
}

// New returns a client for github.com authenticated with token.
func New(token string) (*Client, error) {
	rest, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: token, Timeout: 60 * time.Second})
	if err != nil {
		return nil, err
	}
	return &Client{rest: rest}, nil
}

// Issue is an issue (not a pull request).
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// Comment is an issue comment.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// Issue returns issue n of repo ("owner/name").
func (c *Client) Issue(ctx context.Context, repo string, n int) (Issue, error) {
	var is Issue
	err := c.rest.DoWithContext(ctx, http.MethodGet, fmt.Sprintf("repos/%s/issues/%d", repo, n), nil, &is)
	return is, err
}

// OpenIssues returns every open issue of repo, pull requests excluded.
func (c *Client) OpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	var out []Issue
	err := pages(ctx, c, fmt.Sprintf("repos/%s/issues?state=open&per_page=100", repo), func(page []json.RawMessage) error {
		for _, raw := range page {
			var item struct {
				Issue
				PullRequest json.RawMessage `json:"pull_request"`
			}
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			if item.PullRequest == nil {
				out = append(out, item.Issue)
			}
		}
		return nil
	})
	return out, err
}

// Comments returns every comment of issue n of repo, oldest first.
func (c *Client) Comments(ctx context.Context, repo string, n int) ([]Comment, error) {
	var out []Comment
	err := pages(ctx, c, fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", repo, n), func(page []json.RawMessage) error {
		for _, raw := range page {
			var cm Comment
			if err := json.Unmarshal(raw, &cm); err != nil {
				return err
			}
			out = append(out, cm)
		}
		return nil
	})
	return out, err
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// pages GETs path and follows the Link rel="next" pages, handing each page's items to fn.
func pages(ctx context.Context, c *Client, path string, fn func([]json.RawMessage) error) error {
	for path != "" {
		resp, err := c.rest.RequestWithContext(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		var page []json.RawMessage
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("GET %s: %w", path, err)
		}
		if err := fn(page); err != nil {
			return fmt.Errorf("GET %s: %w", path, err)
		}
		path = ""
		if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			path = m[1]
		}
	}
	return nil
}
