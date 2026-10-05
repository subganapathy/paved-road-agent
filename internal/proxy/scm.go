package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/subganapathy/paved-road-agent/internal/github"
)

// scmSlot is source control: reads scoped to one org, plus the three
// writes to the pull request under review (M5; refused until then).
type scmSlot struct {
	org   string
	api   string
	token string
	http  *http.Client
	gh    *github.Client
	allow map[string]bool // repos where the bot may commit
}

func newSCM(cfg *SCMSlot) (*scmSlot, error) {
	tok, err := Credential(cfg.Auth)
	if err != nil {
		return nil, err
	}
	s := &scmSlot{org: cfg.Org, api: trimSlash(cfg.API), token: tok, http: &http.Client{Timeout: 60 * time.Second}, allow: map[string]bool{}}
	s.gh = &github.Client{Token: tok, HTTP: s.http, Base: s.api}
	for _, r := range cfg.AllowBotCommits {
		s.allow[r] = true
	}
	return s, nil
}

var repoNameRe = regexp.MustCompile(`^[\w.-]+$`)

// repo resolves "name" or "org/name" to the org's repository, refusing
// anything outside the org. This is the scope boundary of the slot.
func (s *scmSlot) repo(in string) (string, error) {
	in = strings.TrimSpace(in)
	if i := strings.IndexByte(in, '/'); i >= 0 {
		if in[:i] != s.org {
			return "", fmt.Errorf("repository %q is outside the org %s", in, s.org)
		}
		in = in[i+1:]
	}
	if !repoNameRe.MatchString(in) {
		return "", fmt.Errorf("repository name %q is malformed", in)
	}
	return in, nil
}

func (s *scmSlot) get(ctx context.Context, path string, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.api+path, nil)
	if err != nil {
		return nil, err
	}
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	return s.http.Do(req)
}

func (s *scmSlot) getJSON(ctx context.Context, path string, out any) (int, error) {
	resp, err := s.get(ctx, path, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp.StatusCode, json.Unmarshal(b, out)
}

// Requests.
type (
	searchCodeReq struct {
		Query string
		Repo  string // optional: narrow to one repository
	}
	readReq struct{ Repo, Ref, Path string }
	prReq   struct {
		Repo   string
		Number int
	}
	openPRReq struct{ Repo string }
)

// searchCode is the org finder's eyes: code search scoped to the org.
func (s *scmSlot) searchCode(ctx context.Context, r searchCodeReq) (any, int, error) {
	if strings.TrimSpace(r.Query) == "" {
		return nil, 0, fmt.Errorf("query is required")
	}
	q := r.Query + " org:" + s.org
	if r.Repo != "" {
		name, err := s.repo(r.Repo)
		if err != nil {
			return nil, 0, err
		}
		q = r.Query + " repo:" + s.org + "/" + name
	}
	var out struct {
		Total int `json:"total_count"`
		Items []struct {
			Path string `json:"path"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		} `json:"items"`
	}
	status, err := s.getJSON(ctx, "/search/code?per_page=30&q="+url.QueryEscape(q), &out)
	if err != nil {
		return nil, status, err
	}
	type hit struct {
		Repo string `json:"repo"`
		Path string `json:"path"`
	}
	hits := make([]hit, 0, len(out.Items))
	for _, it := range out.Items {
		hits = append(hits, hit{Repo: it.Repo.FullName, Path: it.Path})
	}
	return map[string]any{"total": out.Total, "hits": hits}, status, nil
}

// read returns one file at a ref.
func (s *scmSlot) read(ctx context.Context, r readReq) (any, int, error) {
	name, err := s.repo(r.Repo)
	if err != nil {
		return nil, 0, err
	}
	if r.Path == "" || strings.HasPrefix(r.Path, "/") || strings.Contains(r.Path, "..") {
		return nil, 0, fmt.Errorf("path must be relative and inside the repository")
	}
	ref := r.Ref
	if ref == "" {
		ref = "HEAD"
	}
	var body struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int    `json:"size"`
		SHA      string `json:"sha"`
	}
	p := fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", url.PathEscape(s.org), url.PathEscape(name), escapePath(r.Path), url.QueryEscape(ref))
	status, err := s.getJSON(ctx, p, &body)
	if err != nil {
		return nil, status, err
	}
	if body.Encoding != "base64" {
		return nil, status, fmt.Errorf("%s is not a file (or is too large to read inline)", r.Path)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.Content, "\n", ""))
	if err != nil {
		return nil, status, err
	}
	return map[string]any{"repo": s.org + "/" + name, "ref": ref, "path": r.Path, "sha": body.SHA, "content": string(raw)}, status, nil
}

// tarball streams the repository at a ref for the worker to unpack. The
// proxy's token does the fetch; the sandbox never holds one.
func (s *scmSlot) tarball(ctx context.Context, repo, ref string, max int64, w io.Writer) (int, int64, error) {
	name, err := s.repo(repo)
	if err != nil {
		return 0, 0, err
	}
	if ref == "" {
		ref = "HEAD"
	}
	resp, err := s.get(ctx, fmt.Sprintf("/repos/%s/%s/tarball/%s", url.PathEscape(s.org), url.PathEscape(name), url.PathEscape(ref)), "application/vnd.github+json")
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return resp.StatusCode, 0, fmt.Errorf("tarball %s@%s: HTTP %d: %s", name, ref, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if n > max {
		return resp.StatusCode, n, fmt.Errorf("tarball exceeds %d bytes", max)
	}
	return resp.StatusCode, n, err
}

// pr loads a pull request as the change under review.
func (s *scmSlot) pr(ctx context.Context, r prReq) (any, int, error) {
	name, err := s.repo(r.Repo)
	if err != nil {
		return nil, 0, err
	}
	loaded, err := s.gh.Load(ctx, github.PR{Owner: s.org, Repo: name, Number: r.Number})
	if err != nil {
		return nil, 0, err
	}
	return loaded, 200, nil
}

// repos lists the org's repositories: the org finder's fallback when code
// search is not indexed (small or new orgs often are not).
func (s *scmSlot) repos(ctx context.Context) (any, int, error) {
	type repo struct {
		Name        string   `json:"name"`
		Description string   `json:"description,omitempty"`
		Topics      []string `json:"topics,omitempty"`
		Language    string   `json:"language,omitempty"`
		Default     string   `json:"default_branch"`
		Pushed      string   `json:"pushed_at"`
		Archived    bool     `json:"archived,omitempty"`
	}
	var out []repo
	for page := 1; page <= 10; page++ {
		var batch []struct {
			Name          string   `json:"name"`
			Description   string   `json:"description"`
			Topics        []string `json:"topics"`
			Language      string   `json:"language"`
			DefaultBranch string   `json:"default_branch"`
			PushedAt      string   `json:"pushed_at"`
			Archived      bool     `json:"archived"`
		}
		status, err := s.getJSON(ctx, fmt.Sprintf("/orgs/%s/repos?per_page=100&page=%d&sort=pushed", url.PathEscape(s.org), page), &batch)
		if err != nil {
			return nil, status, err
		}
		for _, b := range batch {
			out = append(out, repo{b.Name, b.Description, b.Topics, b.Language, b.DefaultBranch, b.PushedAt, b.Archived})
		}
		if len(batch) < 100 {
			break
		}
	}
	return out, 200, nil
}

// openPRs lists the repository's open pull requests, for cross-PR impact.
func (s *scmSlot) openPRs(ctx context.Context, r openPRReq) (any, int, error) {
	name, err := s.repo(r.Repo)
	if err != nil {
		return nil, 0, err
	}
	var prs []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		UpdatedAt string `json:"updated_at"`
	}
	status, err := s.getJSON(ctx, fmt.Sprintf("/repos/%s/%s/pulls?state=open&per_page=50", url.PathEscape(s.org), url.PathEscape(name)), &prs)
	if err != nil {
		return nil, status, err
	}
	type pr struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Author  string `json:"author"`
		Head    string `json:"head"`
		Updated string `json:"updated"`
	}
	out := make([]pr, 0, len(prs))
	for _, p := range prs {
		out = append(out, pr{p.Number, p.Title, p.User.Login, p.Head.SHA, p.UpdatedAt})
	}
	return out, status, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
