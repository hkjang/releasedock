package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/jackc/pgx/v5"
)

type guidePost struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Summary   string    `json:"summary"`
	Body      string    `json:"body"`
	Category  string    `json:"category"`
	Pinned    bool      `json:"pinned"`
	SortOrder int       `json:"sortOrder"`
	Published bool      `json:"published"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// guideReadingOrder puts pinned posts first, then the administrator's ordering,
// then newest. The board is a reading sequence rather than a feed.
const guideReadingOrder = ` ORDER BY pinned DESC, sort_order, created_at DESC`

const guideColumns = `post.id::text,post.title,post.summary,post.body,post.category,post.pinned,
	post.sort_order,post.published,COALESCE(author.display_name,''),post.created_at,post.updated_at`

func scanGuidePost(row pgx.Row) (guidePost, error) {
	var post guidePost
	err := row.Scan(&post.ID, &post.Title, &post.Summary, &post.Body, &post.Category, &post.Pinned,
		&post.SortOrder, &post.Published, &post.Author, &post.CreatedAt, &post.UpdatedAt)
	return post, err
}

// listGuides serves the reader-facing board. Drafts are invisible here; an
// administrator reviews them through the admin listing.
func (s *Server) listGuides(w http.ResponseWriter, r *http.Request) {
	category := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("category")))
	switch category {
	case "", "GUIDE", "NOTICE", "FAQ":
	default:
		writeError(w, http.StatusBadRequest, "invalid_filter", "category must be GUIDE, NOTICE, or FAQ")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT `+guideColumns+`
		FROM guide_posts post LEFT JOIN users author ON author.id=post.created_by
		WHERE post.published AND ($1='' OR post.category=$1)`+guideReadingOrder+` LIMIT 200`, category)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not list guides")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		post, err := scanGuidePost(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database_error", "could not list guides")
			return
		}
		// The list omits the body so a board with long guides stays small.
		items = append(items, map[string]any{
			"id": post.ID, "title": post.Title, "summary": post.Summary,
			"category": post.Category, "pinned": post.Pinned, "author": post.Author,
			"createdAt": post.CreatedAt, "updatedAt": post.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) getGuide(w http.ResponseWriter, r *http.Request) {
	post, err := scanGuidePost(s.store.Pool.QueryRow(r.Context(), `SELECT `+guideColumns+`
		FROM guide_posts post LEFT JOIN users author ON author.id=post.created_by
		WHERE post.id=$1 AND post.published`, r.PathValue("id")))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "게시글을 찾을 수 없습니다")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not load guide")
		return
	}
	writeJSON(w, http.StatusOK, post)
}

type guidePostInput struct {
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	Body      string `json:"body"`
	Category  string `json:"category"`
	Pinned    *bool  `json:"pinned"`
	SortOrder *int   `json:"sortOrder"`
	Published *bool  `json:"published"`
}

func normalizeGuidePost(input *guidePostInput) (guidePost, error) {
	post := guidePost{
		Title:     strings.TrimSpace(input.Title),
		Summary:   strings.TrimSpace(input.Summary),
		Body:      input.Body,
		Category:  strings.ToUpper(strings.TrimSpace(input.Category)),
		SortOrder: 100,
		Published: true,
	}
	if post.Category == "" {
		post.Category = "GUIDE"
	}
	if post.Title == "" || len(post.Title) > 200 {
		return post, errors.New("제목은 1~200자여야 합니다")
	}
	if len(post.Summary) > 500 {
		return post, errors.New("요약은 500자를 넘을 수 없습니다")
	}
	if len(post.Body) > 200000 {
		return post, errors.New("본문이 너무 깁니다")
	}
	switch post.Category {
	case "GUIDE", "NOTICE", "FAQ":
	default:
		return post, errors.New("분류는 GUIDE, NOTICE, FAQ 중 하나여야 합니다")
	}
	if input.SortOrder != nil {
		post.SortOrder = *input.SortOrder
	}
	if post.SortOrder < 0 || post.SortOrder > 100000 {
		return post, errors.New("정렬 순서는 0 이상 100000 이하여야 합니다")
	}
	if input.Pinned != nil {
		post.Pinned = *input.Pinned
	}
	if input.Published != nil {
		post.Published = *input.Published
	}
	return post, nil
}

// listAdminGuides includes drafts, which is the only difference from the
// reader-facing listing.
func (s *Server) listAdminGuides(w http.ResponseWriter, r *http.Request) {
	limit, offset := pagination(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	filter := `($1='' OR post.title ILIKE '%'||$1||'%' OR post.summary ILIKE '%'||$1||'%')`
	var total int
	if err := s.store.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM guide_posts post WHERE `+filter, search).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not count guides")
		return
	}
	rows, err := s.store.Pool.Query(r.Context(), `SELECT `+guideColumns+`
		FROM guide_posts post LEFT JOIN users author ON author.id=post.created_by
		WHERE `+filter+guideReadingOrder+` LIMIT $2 OFFSET $3`, search, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "could not list guides")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		post, err := scanGuidePost(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database_error", "could not list guides")
			return
		}
		items = append(items, map[string]any{
			"id": post.ID, "title": post.Title, "summary": post.Summary, "body": post.Body,
			"category": post.Category, "pinned": post.Pinned, "sortOrder": post.SortOrder,
			"published": post.Published, "author": post.Author,
			"createdAt": post.CreatedAt, "updatedAt": post.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, page(items, total, limit, offset))
}

func (s *Server) createGuide(w http.ResponseWriter, r *http.Request) {
	var input guidePostInput
	if !decodeJSON(w, r, &input) {
		return
	}
	post, err := normalizeGuidePost(&input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_guide", err.Error())
		return
	}
	id, err := secure.NewID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "id_error", "could not allocate an identifier")
		return
	}
	p, _ := principalFrom(r)
	if _, err := s.store.Pool.Exec(r.Context(), `INSERT INTO guide_posts
		(id,title,summary,body,category,pinned,sort_order,published,created_by,updated_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`,
		id, post.Title, post.Summary, post.Body, post.Category, post.Pinned,
		post.SortOrder, post.Published, p.UserID); err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "게시글을 저장하지 못했습니다")
		return
	}
	post.ID = id
	s.store.Audit(r.Context(), p.UserID, "guide.create", "guide", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, http.StatusCreated, post)
}

func (s *Server) updateGuide(w http.ResponseWriter, r *http.Request) {
	var input guidePostInput
	if !decodeJSON(w, r, &input) {
		return
	}
	post, err := normalizeGuidePost(&input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_guide", err.Error())
		return
	}
	id := r.PathValue("id")
	p, _ := principalFrom(r)
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE guide_posts SET
		title=$2,summary=$3,body=$4,category=$5,pinned=$6,sort_order=$7,published=$8,
		updated_by=$9,updated_at=now() WHERE id=$1`,
		id, post.Title, post.Summary, post.Body, post.Category, post.Pinned,
		post.SortOrder, post.Published, p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database_error", "게시글을 저장하지 못했습니다")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "게시글을 찾을 수 없습니다")
		return
	}
	post.ID = id
	s.store.Audit(r.Context(), p.UserID, "guide.update", "guide", id, "success", remoteIP(r), r.UserAgent(), nil)
	writeJSON(w, http.StatusOK, post)
}

func (s *Server) deleteGuide(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tag, err := s.store.Pool.Exec(r.Context(), `DELETE FROM guide_posts WHERE id=$1`, id)
	if err != nil || tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "게시글을 찾을 수 없습니다")
		return
	}
	p, _ := principalFrom(r)
	s.store.Audit(r.Context(), p.UserID, "guide.delete", "guide", id, "success", remoteIP(r), r.UserAgent(), nil)
	w.WriteHeader(http.StatusNoContent)
}
