// Package github loads a pull request as a change: the files with their
// patches, and both sides of the small configuration files the classifier
// diffs semantically. REST only, no SDK; a token is optional for public repos.
package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/subganapathy/paved-road-agent/internal/change"
)

// PR locates a pull request.
type PR struct {
	Owner, Repo string
	Number      int
}

var prRef = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)#(\d+)$`)

// ParsePR accepts owner/repo#N.
func ParsePR(s string) (PR, error) {
	m := prRef.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return PR{}, fmt.Errorf("want owner/repo#N, got %q", s)
	}
	n, _ := strconv.Atoi(m[3])
	return PR{Owner: m[1], Repo: m[2], Number: n}, nil
}

// Loaded is a pull request as the agent sees it.
type Loaded struct {
	Change      change.Change
	Title, Body string
	HeadSHA     string
	HeadRef     string // the PR branch, where answers and proposals are committed
	BaseSHA     string
	RepoURL     string // https://github.com/owner/repo
	HTMLURL     string
}

// Client talks to the GitHub REST API.
type Client struct {
	Token string
	HTTP  *http.Client
	Base  string // default https://api.github.com
}

func (c *Client) get(ctx context.Context, p string, out any) error {
	base := c.Base
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+p, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: HTTP %d: %s", p, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.Unmarshal(b, out)
}

// Load fetches the pull request, its files and, for configuration files
// small enough to diff semantically, their contents on both sides.
func (c *Client) Load(ctx context.Context, pr PR) (*Loaded, error) {
	var meta struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo struct {
				HTMLURL string `json:"html_url"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
	}
	repo := fmt.Sprintf("/repos/%s/%s", url.PathEscape(pr.Owner), url.PathEscape(pr.Repo))
	if err := c.get(ctx, fmt.Sprintf("%s/pulls/%d", repo, pr.Number), &meta); err != nil {
		return nil, err
	}
	out := &Loaded{
		Title: meta.Title, Body: meta.Body, HTMLURL: meta.HTMLURL,
		HeadSHA: meta.Head.SHA, HeadRef: meta.Head.Ref, BaseSHA: meta.Base.SHA,
		RepoURL: fmt.Sprintf("https://github.com/%s/%s", pr.Owner, pr.Repo),
	}
	out.Change.Ref = fmt.Sprintf("%s/%s#%d", pr.Owner, pr.Repo, pr.Number)

	for page := 1; ; page++ {
		var files []struct {
			Filename string `json:"filename"`
			Status   string `json:"status"`
			Patch    string `json:"patch"`
		}
		if err := c.get(ctx, fmt.Sprintf("%s/pulls/%d/files?per_page=100&page=%d", repo, pr.Number, page), &files); err != nil {
			return nil, err
		}
		for _, f := range files {
			cf := change.File{Path: f.Filename, Status: status(f.Status), Patch: f.Patch}
			if semantic(f.Filename) {
				if f.Status != "added" {
					cf.Before, _ = c.contents(ctx, repo, f.Filename, meta.Base.SHA)
				}
				if f.Status != "removed" {
					cf.After, _ = c.contents(ctx, repo, f.Filename, meta.Head.SHA)
				}
			}
			out.Change.Files = append(out.Change.Files, cf)
		}
		if len(files) < 100 {
			break
		}
	}
	return out, nil
}

// status maps GitHub's file status onto ours.
func status(s string) change.Status {
	switch s {
	case "added":
		return change.Added
	case "removed":
		return change.Deleted
	case "renamed":
		return change.Renamed
	}
	return change.Modified
}

// semantic says which files are worth fetching whole so the classifier can
// diff keys rather than hunks.
func semantic(p string) bool {
	base := path.Base(p)
	switch {
	case base == "service.yaml", strings.HasSuffix(base, ".tf"), strings.HasSuffix(base, ".tfvars"):
		return true
	case strings.HasPrefix(p, "deploy/") && (strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml")):
		return true
	}
	return false
}

// Contents returns one file at a ref, for callers outside the package.
func (c *Client) Contents(ctx context.Context, owner, repo, p, ref string) (string, error) {
	return c.contents(ctx, fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo)), p, ref)
}

func (c *Client) contents(ctx context.Context, repo, p, ref string) (string, error) {
	var body struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int    `json:"size"`
	}
	if err := c.get(ctx, fmt.Sprintf("%s/contents/%s?ref=%s", repo, escapePath(p), url.QueryEscape(ref)), &body); err != nil {
		return "", err
	}
	if body.Encoding != "base64" || body.Size > 256<<10 {
		return "", fmt.Errorf("%s: not inline", p)
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.Content, "\n", ""))
	return string(b), err
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// OpenPR is one open pull request, as polling needs it.
type OpenPR struct {
	Number  int
	HeadSHA string
	HeadRef string
	Title   string
}

// OpenPRs lists a repository's open pull requests.
func (c *Client) OpenPRs(ctx context.Context, owner, repo string) ([]OpenPR, error) {
	var raw []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Head   struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/pulls?state=open&per_page=50", url.PathEscape(owner), url.PathEscape(repo)), &raw); err != nil {
		return nil, err
	}
	out := make([]OpenPR, 0, len(raw))
	for _, r := range raw {
		out = append(out, OpenPR{Number: r.Number, HeadSHA: r.Head.SHA, HeadRef: r.Head.Ref, Title: r.Title})
	}
	return out, nil
}
