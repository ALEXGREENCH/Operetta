package proxy

import "testing"

func TestBuildURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		base   string
		action string
		get    string
		want   string
	}{
		{
			name:   "https wiki random",
			base:   "https://ru.wikipedia.org/wiki/%D0%97%D0%B0%D0%B3%D0%BB%D0%B0%D0%B2%D0%BD%D0%B0%D1%8F_%D1%81%D1%82%D1%80%D0%B0%D0%BD%D0%B8%D1%86%D0%B0",
			action: "/wiki/%D0%A1%D0%BB%D1%83%D1%87%D0%B0%D0%B9%D0%BD%D0%B0%D1%8F_%D1%81%D1%82%D1%80%D0%B0%D0%BD%D0%B8%D1%86%D0%B0",
			want:   "https://ru.wikipedia.org/wiki/%D0%A1%D0%BB%D1%83%D1%87%D0%B0%D0%B9%D0%BD%D0%B0%D1%8F_%D1%81%D1%82%D1%80%D0%B0%D0%BD%D0%B8%D1%86%D0%B0",
		},
		{
			name:   "relative same dir",
			base:   "https://example.com/path/dir/page.html",
			action: "next.html",
			want:   "https://example.com/path/dir/next.html",
		},
		{
			name:   "root relative",
			base:   "https://example.com/path/index.html",
			action: "/other/page",
			want:   "https://example.com/other/page",
		},
		{
			name: "append get",
			base: "https://example.com/path",
			get:  "a=b",
			want: "https://example.com/path?a=b",
		},
		{
			name: "append get to existing query",
			base: "https://example.com/path?x=1",
			get:  "y=2",
			want: "https://example.com/path?x=1&y=2",
		},
		{
			name:   "action with query and get",
			base:   "https://example.com/start",
			action: "/foo?x=1",
			get:    "y=2",
			want:   "https://example.com/foo?x=1&y=2",
		},
		{
			name:   "absolute action",
			base:   "https://example.com/path",
			action: "http://other.com/page",
			want:   "http://other.com/page",
		},
		{
			name:   "double encoded base",
			base:   "https%3A%2F%2Fexample.com%2Fdir%2Fpage.html",
			action: "next.html",
			want:   "https://example.com/dir/next.html",
		},
		{
			name: "preserve percent query",
			base: "https://example.com/path",
			get:  "title=%D0%A1",
			want: "https://example.com/path?title=%D0%A1",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := buildURL(tc.base, tc.action, tc.get); got != tc.want {
				t.Fatalf("buildURL(%q, %q, %q) = %q, want %q", tc.base, tc.action, tc.get, got, tc.want)
			}
		})
	}
}
func TestNormalizeObmlURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "double encoded wikipedia",
			in:   "/obml/0/http://ru.wikipedia.org/wiki/%25D0%25A1%25D0%25BB%25D1%2583%25D1%2587%25D0%25B0%25D0%25B9%25D0%25BD%25D0%25B0%25D1%258F:%25D0%25A1%25D0%25BB%25D1%2583%25D1%2587%25D0%25B0%25D0%25B9%25D0%25BD%25D0%25B0%25D1%258F_%25D1%2581%25D1%2582%25D1%2580%25D0%25B0%25D0%25BD%25D0%25B8%25D1%2586%25D0%25B0",
			want: "http://ru.wikipedia.org/wiki/%D0%A1%D0%BB%D1%83%D1%87%D0%B0%D0%B9%D0%BD%D0%B0%D1%8F:%D0%A1%D0%BB%D1%83%D1%87%D0%B0%D0%B9%D0%BD%D0%B0%D1%8F_%D1%81%D1%82%D1%80%D0%B0%D0%BD%D0%B8%D1%86%D0%B0",
		},
		{
			name: "https preserved",
			in:   "/obml/0/https%3A%2F%2Fexample.com%2Fpath",
			want: "https://example.com/path",
		},
		{
			name: "no scheme adds http",
			in:   "example.com/path",
			want: "http://example.com/path",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeObmlURL(tc.in); got != tc.want {
				t.Fatalf("normalizeObmlURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
