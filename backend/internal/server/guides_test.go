package server

import (
	"strings"
	"testing"
)

func TestNormalizeGuidePostAppliesDefaults(t *testing.T) {
	input := guidePostInput{Title: "  심플 모드로 배포하기  ", Body: "본문"}
	post, err := normalizeGuidePost(&input)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if post.Title != "심플 모드로 배포하기" {
		t.Fatalf("title was not trimmed: %q", post.Title)
	}
	// A post with no category is a guide, and a new post is visible by default.
	if post.Category != "GUIDE" || post.SortOrder != 100 || !post.Published {
		t.Fatalf("unexpected defaults: %+v", post)
	}
}

func TestNormalizeGuidePostRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		input guidePostInput
	}{
		{"empty title", guidePostInput{Title: "   ", Body: "x"}},
		{"long title", guidePostInput{Title: strings.Repeat("가", 201), Body: "x"}},
		{"long summary", guidePostInput{Title: "제목", Summary: strings.Repeat("가", 501)}},
		{"unknown category", guidePostInput{Title: "제목", Category: "BLOG"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := testCase.input
			if _, err := normalizeGuidePost(&input); err == nil {
				t.Fatal("expected the input to be rejected")
			}
		})
	}
}

func TestNormalizeGuidePostBoundsSortOrder(t *testing.T) {
	negative := -1
	tooLarge := 100001
	fine := 5
	for _, order := range []int{negative, tooLarge} {
		input := guidePostInput{Title: "제목", SortOrder: &order}
		if _, err := normalizeGuidePost(&input); err == nil {
			t.Fatalf("expected sortOrder %d to be rejected", order)
		}
	}
	input := guidePostInput{Title: "제목", SortOrder: &fine}
	post, err := normalizeGuidePost(&input)
	if err != nil || post.SortOrder != fine {
		t.Fatalf("expected sortOrder %d to be accepted, got %+v err=%v", fine, post, err)
	}
}

// The body is stored verbatim; the browser renders a Markdown subset and never
// interprets HTML, so nothing is stripped here.
func TestNormalizeGuidePostKeepsBodyVerbatim(t *testing.T) {
	body := "# 제목\n\n<script>alert(1)</script>\n\n- 항목"
	input := guidePostInput{Title: "제목", Body: body}
	post, err := normalizeGuidePost(&input)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if post.Body != body {
		t.Fatalf("body was altered: %q", post.Body)
	}
}
