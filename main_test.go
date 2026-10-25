package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func learningCache(t *testing.T, path string) *PreCache {
	t.Helper()
	return &PreCache{cfg: Config{LearnPath: path}, out: &Printer{}}
}

func slowResult(u string) Result {
	return Result{Page: Page{RawURL: u, URL: u}, Timeout: true, Attempts: 1, Class: ClassException}
}

func normalResult(u string) Result {
	return Result{Page: Page{RawURL: u, URL: u}, Status: http.StatusOK, Attempts: 1, Class: ClassNoHeader}
}

func readHistory(t *testing.T, path string) map[string]slowPageHistory {
	t.Helper()
	data, err := os.ReadFile(path + ".history.json")
	if err != nil {
		t.Fatal(err)
	}
	var history map[string]slowPageHistory
	if err := json.Unmarshal(data, &history); err != nil {
		t.Fatal(err)
	}
	return history
}

func TestLearningRequiresThreeCalendarDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slow-pages.txt")
	u := "https://example.com/slow"
	// 跨月、跨年也必须按自然日连续计数；时间相差不到 24 小时仍是两天。
	days := []time.Time{
		time.Date(2026, 12, 31, 23, 59, 0, 0, time.Local),
		time.Date(2027, 1, 1, 0, 1, 0, 0, time.Local),
		time.Date(2027, 1, 2, 0, 1, 0, 0, time.Local),
	}
	for i, day := range days {
		// 每次重新构造，验证累计依据磁盘记录而不是内存。
		pc := learningCache(t, path)
		pc.saveLearnedAt([]Result{slowResult(u), slowResult(u)}, day)
		pc.saveLearnedAt([]Result{slowResult(u)}, day)
		if got := readHistory(t, path)[u].Days; got != i+1 {
			t.Fatalf("day %d: got %d days", i+1, got)
		}
		learned := pc.loadLearned()
		if i < 2 && len(learned) != 0 {
			t.Fatalf("learned too soon: %v", learned)
		}
		if i == 2 && (len(learned) != 1 || learned[0] != u) {
			t.Fatalf("third day did not learn exactly once: %v", learned)
		}
		if i < 2 {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("exclusion file created before confirmation: %v", err)
			}
		}
	}
}

func TestLearningResets(t *testing.T) {
	u := "https://example.com/slow"
	day := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	tests := []struct {
		name   string
		middle []Result
		offset int
		want   int
	}{
		{"normal response", []Result{normalResult(u)}, 2, 1},
		{"timeout then normal in same batch", []Result{slowResult(u), normalResult(u)}, 2, 1},
		{"normal then timeout in same batch", []Result{normalResult(u), slowResult(u)}, 2, 1},
		{"missed day", nil, 2, 1},
		{"other network error is not a timeout day", []Result{{Page: Page{RawURL: u}, Class: ClassException, Attempts: 1}}, 2, 1},
		{"cancelled or unexecuted result is ignored", []Result{{Page: Page{RawURL: u}, Class: ClassException}, {}}, 1, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "slow-pages.txt")
			pc := learningCache(t, path)
			pc.saveLearnedAt([]Result{slowResult(u)}, day)
			pc.saveLearnedAt(tt.middle, day.AddDate(0, 0, 1))
			pc.saveLearnedAt([]Result{slowResult(u)}, day.AddDate(0, 0, tt.offset))
			if got := readHistory(t, path)[u].Days; got != tt.want {
				t.Fatalf("got %d days, want %d", got, tt.want)
			}
			if got := pc.loadLearned(); len(got) != 0 {
				t.Fatalf("unexpected exclusions: %v", got)
			}
		})
	}
}

func TestLearningNormalResponseDisqualifiesSameDay(t *testing.T) {
	u := "https://example.com/slow"
	for _, normalFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout first", true: "normal first"}[normalFirst], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "slow-pages.txt")
			pc := learningCache(t, path)
			day := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
			pc.saveLearnedAt([]Result{slowResult(u)}, day)
			if normalFirst {
				pc.saveLearnedAt([]Result{normalResult(u)}, day.AddDate(0, 0, 1))
			}
			pc.saveLearnedAt([]Result{slowResult(u)}, day.AddDate(0, 0, 1))
			if !normalFirst {
				pc.saveLearnedAt([]Result{normalResult(u)}, day.AddDate(0, 0, 1))
			}
			pc.saveLearnedAt([]Result{slowResult(u)}, day.AddDate(0, 0, 1))
			if got := readHistory(t, path)[u].Days; got != 0 {
				t.Fatalf("same day normal response must reset count, got %d", got)
			}
			pc.saveLearnedAt([]Result{slowResult(u)}, day.AddDate(0, 0, 2))
			if got := readHistory(t, path)[u].Days; got != 1 {
				t.Fatalf("next day must restart at 1, got %d", got)
			}
		})
	}
}

func TestLearningCalendarDayAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "slow-pages.txt")
	pc := learningCache(t, path)
	u := "https://example.com/slow"
	// 夏令时结束当天有 25 小时，不能用减去 24 小时判断昨天。
	for _, d := range []int{31, 1, 2} {
		month := time.November
		if d == 31 {
			month = time.October
		}
		pc.saveLearnedAt([]Result{slowResult(u)}, time.Date(2026, month, d, 0, 30, 0, 0, loc))
	}
	if got := pc.loadLearned(); len(got) != 1 || got[0] != u {
		t.Fatalf("calendar days across DST should be consecutive: %v", got)
	}
}

func TestLearningKeepsURLsIndependentAndLegacyEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.txt")
	legacy := "# manually maintained\nhttps://example.com/old" // 无末尾换行
	if err := os.WriteFile(path, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}
	pc := learningCache(t, path)
	u := "https://example.com/slow"
	other := "https://other.example.com/slow"
	day := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	for i := 0; i < 3; i++ {
		results := []Result{slowResult(u), normalResult(other)}
		if i == 2 {
			results[1] = slowResult(other)
		}
		// 连接重写后的地址不能取代原始 URL 作为学习键。
		results[0].Page.URL = "http://127.0.0.1/slow"
		pc.saveLearnedAt(results, day.AddDate(0, 0, i))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := legacy + "\n" + u + "\n"; string(data) != want {
		t.Fatalf("got %q, want %q", data, want)
	}
	if got := readHistory(t, path)[other].Days; got != 1 {
		t.Fatalf("different URL inherited another URL's count: %d", got)
	}
}

func TestLearningDisabledOrInvalidHistory(t *testing.T) {
	for _, name := range []string{"disabled", "empty path", "invalid history", "null history", "unreadable history", "unwritable history"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "slow-pages.txt")
			pc := learningCache(t, path)
			var original string
			switch name {
			case "disabled":
				pc.cfg.NoLearn = true
			case "empty path":
				pc.cfg.LearnPath = ""
			case "invalid history", "null history":
				original = "{broken"
				if name == "null history" {
					original = "null"
				}
				if err := os.WriteFile(path+".history.json", []byte(original), 0644); err != nil {
					t.Fatal(err)
				}
			case "unreadable history":
				if err := os.Mkdir(path+".history.json", 0755); err != nil {
					t.Fatal(err)
				}
			case "unwritable history":
				path = filepath.Join(t.TempDir(), "missing", "slow-pages.txt")
				pc.cfg.LearnPath = path
			}
			for i := 0; i < 3; i++ {
				pc.saveLearnedAt([]Result{slowResult("https://example.com/slow")}, time.Date(2026, 10, 20+i, 12, 0, 0, 0, time.Local))
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("must not learn without reliable history: %v", err)
			}
			if original != "" {
				data, err := os.ReadFile(path + ".history.json")
				if err != nil || string(data) != original {
					t.Fatalf("corrupt history must be preserved: %q, %v", data, err)
				}
			}
			if name == "disabled" || name == "empty path" {
				if _, err := os.Stat(path + ".history.json"); !os.IsNotExist(err) {
					t.Fatalf("learning disabled but history was written: %v", err)
				}
			}
		})
	}
}

func TestStartObservesThenExcludesConfirmedPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slow-pages.txt")
	var slowRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sitemap.xml":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte("<urlset><url><loc>" + server.URL + "/slow</loc></url><url><loc>" + server.URL + "/fast</loc></url></urlset>"))
		case "/slow":
			slowRequests.Add(1)
			<-r.Context().Done()
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	newCache := func() *PreCache {
		pc, err := NewPreCache(Config{Sitemap: server.URL + "/sitemap.xml", LearnPath: path, Timeout: 100 * time.Millisecond, Concurrency: 1}, &Printer{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pc.transport.CloseIdleConnections)
		return pc
	}
	u := server.URL + "/slow"
	// 先通过真实请求确认第一次超时后没有排除。
	if code, err := newCache().Start(context.Background()); err != nil || code != 0 {
		t.Fatalf("first run: code %d, err %v", code, err)
	}
	if got := learningCache(t, path).loadLearned(); len(got) != 0 {
		t.Fatalf("first timeout was excluded: %v", got)
	}
	// 模拟前两天的观察，再通过实际 Start 验证第三天确认及下次跳过。
	if err := os.Remove(path + ".history.json"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pc := learningCache(t, path)
	pc.saveLearnedAt([]Result{slowResult(u)}, now.AddDate(0, 0, -2))
	pc.saveLearnedAt([]Result{slowResult(u)}, now.AddDate(0, 0, -1))
	if code, err := newCache().Start(context.Background()); err != nil || code != 0 {
		t.Fatalf("third day: code %d, err %v", code, err)
	}
	if got := pc.loadLearned(); len(got) != 1 || got[0] != u {
		t.Fatalf("third day did not confirm page: %v", got)
	}
	if got := slowRequests.Load(); got != 2 {
		t.Fatalf("observing pages must still be requested: got %d requests, want 2", got)
	}
	next := newCache()
	if code, err := next.Start(context.Background()); err != nil || code != 0 {
		t.Fatalf("next run: code %d, err %v", code, err)
	}
	if got := strings.Join(next.cfg.Exclude, "\n"); !strings.Contains(got, u) {
		t.Fatalf("confirmed page was not loaded for exclusion: %q", got)
	}
	if got := slowRequests.Load(); got != 2 {
		t.Fatalf("confirmed page was requested again: got %d requests, want 2", got)
	}
}
