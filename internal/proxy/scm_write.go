package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The three writes. Every one of them touches only the pull request under
// review: a comment on it, a status on its head commit, or a file under
// .paved-agent/ on its branch. Nothing else in the slot can write.

// CommentReq posts a comment on a pull request.
type CommentReq struct {
	Repo   string
	Number int
	Body   string
}

// StatusReq sets a commit status, the gate branch protection can require.
type StatusReq struct {
	Repo        string
	SHA         string
	State       string // pending | success | failure | error
	Context     string // e.g. "impact"
	Description string // ≤ 140 chars
	TargetURL   string
}

// CommitFileReq writes one file on a branch, creating or updating it.
type CommitFileReq struct {
	Repo    string
	Branch  string
	Path    string
	Content string
	Message string
}

func (s *scmSlot) send(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, s.api+path, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, resp.StatusCode, nil
}

// Comment posts on the pull request.
func (s *scmSlot) Comment(ctx context.Context, r CommentReq) (any, int, error) {
	name, err := s.repo(r.Repo)
	if err != nil {
		return nil, 0, err
	}
	if r.Number <= 0 || strings.TrimSpace(r.Body) == "" {
		return nil, 0, fmt.Errorf("number and body are required")
	}
	out, status, err := s.send(ctx, "POST", fmt.Sprintf("/repos/%s/%s/issues/%d/comments", url.PathEscape(s.org), url.PathEscape(name), r.Number), map[string]string{"body": r.Body})
	if err != nil {
		return nil, status, err
	}
	var c struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	_ = json.Unmarshal(out, &c)
	return map[string]any{"id": c.ID, "url": c.HTMLURL}, status, nil
}

// Status sets the commit status that gates the pull request.
func (s *scmSlot) Status(ctx context.Context, r StatusReq) (any, int, error) {
	name, err := s.repo(r.Repo)
	if err != nil {
		return nil, 0, err
	}
	switch r.State {
	case "pending", "success", "failure", "error":
	default:
		return nil, 0, fmt.Errorf("state %q is not pending, success, failure or error", r.State)
	}
	if r.SHA == "" {
		return nil, 0, fmt.Errorf("sha is required")
	}
	if r.Context == "" {
		r.Context = "impact"
	}
	if len(r.Description) > 140 {
		r.Description = r.Description[:137] + "..."
	}
	body := map[string]string{"state": r.State, "context": r.Context, "description": r.Description}
	if r.TargetURL != "" {
		body["target_url"] = r.TargetURL
	}
	_, status, err := s.send(ctx, "POST", fmt.Sprintf("/repos/%s/%s/statuses/%s", url.PathEscape(s.org), url.PathEscape(name), url.PathEscape(r.SHA)), body)
	if err != nil {
		return nil, status, err
	}
	return map[string]any{"state": r.State, "context": r.Context}, status, nil
}

// CommitFile creates or updates one file under .paved-agent/ on a branch.
// The path restriction is the policy: nothing else may be written.
func (s *scmSlot) CommitFile(ctx context.Context, r CommitFileReq) (any, int, error) {
	name, err := s.repo(r.Repo)
	if err != nil {
		return nil, 0, err
	}
	if r.Branch == "" || strings.HasPrefix(r.Branch, "refs/") {
		return nil, 0, fmt.Errorf("branch is required, as a plain name")
	}
	if !strings.HasPrefix(r.Path, ".paved-agent/") || strings.Contains(r.Path, "..") {
		return nil, 0, fmt.Errorf("refused: the only writable path is under .paved-agent/, got %q", r.Path)
	}
	if strings.TrimSpace(r.Content) == "" {
		return nil, 0, fmt.Errorf("content is required")
	}
	// The contents API needs the current blob sha to update an existing file.
	var cur struct {
		SHA string `json:"sha"`
	}
	p := fmt.Sprintf("/repos/%s/%s/contents/%s", url.PathEscape(s.org), url.PathEscape(name), escapePath(r.Path))
	if status, err := s.getJSON(ctx, p+"?ref="+url.QueryEscape(r.Branch), &cur); err != nil && status != 404 {
		return nil, status, err
	}
	msg := r.Message
	if msg == "" {
		msg = "paved-agent: update " + r.Path
	}
	body := map[string]string{"message": msg, "content": base64.StdEncoding.EncodeToString([]byte(r.Content)), "branch": r.Branch}
	if cur.SHA != "" {
		body["sha"] = cur.SHA
	}
	out, status, err := s.send(ctx, "PUT", p, body)
	if err != nil {
		return nil, status, err
	}
	var res struct {
		Commit struct {
			SHA     string `json:"sha"`
			HTMLURL string `json:"html_url"`
		} `json:"commit"`
	}
	_ = json.Unmarshal(out, &res)
	return map[string]any{"commit": res.Commit.SHA, "url": res.Commit.HTMLURL, "path": r.Path, "branch": r.Branch}, status, nil
}

// Comments lists a pull request's conversation comments after an id, so
// the controller can find replies to its questions.
func (s *scmSlot) Comments(ctx context.Context, repo string, number int, sinceID int64) ([]PRComment, int, error) {
	name, err := s.repo(repo)
	if err != nil {
		return nil, 0, err
	}
	var raw []struct {
		ID   int64 `json:"id"`
		User struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	}
	status, err := s.getJSON(ctx, fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", url.PathEscape(s.org), url.PathEscape(name), number), &raw)
	if err != nil {
		return nil, status, err
	}
	var out []PRComment
	for _, c := range raw {
		if c.ID > sinceID {
			out = append(out, PRComment{ID: c.ID, Author: c.User.Login, Bot: c.User.Type == "Bot", Body: c.Body, CreatedAt: c.CreatedAt})
		}
	}
	return out, status, nil
}

// PRComment is one conversation comment.
type PRComment struct {
	ID        int64  `json:"id"`
	Author    string `json:"author"`
	Bot       bool   `json:"bot"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// Writes exposes the three writes and the comment listing to the
// controller, which runs in the same process as the proxy.
type Writes interface {
	Comment(ctx context.Context, r CommentReq) (any, int, error)
	Status(ctx context.Context, r StatusReq) (any, int, error)
	CommitFile(ctx context.Context, r CommitFileReq) (any, int, error)
	Comments(ctx context.Context, repo string, number int, sinceID int64) ([]PRComment, int, error)
}

// SCM returns the slot's writes for the controller, or nil when unbound.
func (s *Server) SCM() Writes {
	if s.scm == nil {
		return nil
	}
	return s.scm
}
