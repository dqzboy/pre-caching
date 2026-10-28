package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheStatusClassification(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want Class
	}{
		{"", ClassNoHeader}, {"  ", ClassNoHeader}, {"hit", ClassHit},
		{"MISS", ClassMiss}, {"TCP_MISS", ClassMiss}, {"TCP_MEM_HIT", ClassHit},
		{"EXPIRED", ClassExpired}, {"STALE", ClassStale}, {"UPDATING", ClassUpdating},
		{"REVALIDATED", ClassRevalidated}, {"BYPASS", ClassUncacheable},
		{"DYNAMIC", ClassUncacheable}, {"NOT_HIT", ClassUncacheable},
		{"HIT, MISS", ClassMiss}, {"MISS, HIT", ClassHit}, {"HIT, BYPASS", ClassUncacheable},
	} {
		if got := classifyCache(tt.raw); got != tt.want {
			t.Errorf("%q: got %s, want %s", tt.raw, got, tt.want)
		}
	}
}

func TestCacheStatisticsAndRecheck(t *testing.T) {
	states := []string{"HIT", "MISS", "EXPIRED", "STALE", "UPDATING", "REVALIDATED"}
	var mu sync.Mutex
	visits := make(map[string]int)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sitemap.xml":
			fmt.Fprint(w, "<urlset>")
			for _, state := range states {
				fmt.Fprintf(w, "<url><loc>%s/%s</loc></url>", server.URL, state)
			}
			fmt.Fprint(w, "</urlset>")
		case "/":
			w.Header().Set("X-Cache", "HIT")
		default:
			state := strings.TrimPrefix(r.URL.Path, "/")
			mu.Lock()
			visits[state]++
			n := visits[state]
			mu.Unlock()
			if n > 1 && state != "STALE" {
				state = "HIT"
			}
			w.Header().Set("X-Cache", state)
		}
	}))
	defer server.Close()
	reportPath := filepath.Join(t.TempDir(), "report.json")
	pc := newTestCache(t, Config{Sitemap: server.URL + "/sitemap.xml", CacheHeader: "X-Cache", Recheck: time.Millisecond, JSONPath: reportPath})
	logPath := filepath.Join(t.TempDir(), "output.log")
	pc.out = newPrinter(logPath, 5<<20, 7)
	defer pc.out.Close()
	if code, err := pc.Start(context.Background()); code != 0 || err != nil {
		t.Fatalf("code=%d, err=%v", code, err)
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report jsonReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if report.Counts[strings.ToLower(state)] != 1 || report.CacheStatuses[state] != 1 {
			t.Errorf("lost original %s: %+v", state, report)
		}
	}
	if math.Abs(report.FirstPassHitRate-100.0/6) > 1e-9 || report.HitRate != report.FirstPassHitRate {
		t.Fatalf("first pass rate=%v", report.FirstPassHitRate)
	}
	rc := report.Recheck
	if rc == nil || rc.Total != 5 || rc.Hit != 4 || rc.Stale != 1 || rc.Miss != 0 || rc.NotHit != 1 || rc.HitRate != 80 || rc.CacheStatuses["STALE"] != 1 {
		t.Fatalf("wrong recheck: %+v", rc)
	}
	if visits["HIT"] != 1 {
		t.Fatal("already HIT page was rechecked")
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"首轮请求直接命中率", "旧缓存响应 (STALE)", "原始缓存头分布", "预热后复检命中率", "80.0% (4/5)"} {
		if !strings.Contains(string(logData), text) {
			t.Errorf("missing %q in log", text)
		}
	}
}

func TestLogRotationAndExpiry(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name   string
		age    time.Duration
		max    int64
		backup bool
	}{
		{"size", 0, 4, true}, {"daily", 25 * time.Hour, 1024, true},
		{"expired", 8 * 24 * time.Hour, 1024, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pre cache.log")
			if err := os.WriteFile(path, []byte("old content\n"), 0600); err != nil {
				t.Fatal(err)
			}
			modified := now.Add(-tt.age)
			if err := os.Chtimes(path, modified, modified); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(path)
			p := newPrinter(path, tt.max, 7)
			defer p.Close()
			p.Info("new")
			after, _ := os.Stat(path)
			if !os.SameFile(before, after) {
				t.Fatal("active inode changed")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "new\n" {
				t.Fatalf("active log=%q err=%v", data, err)
			}
			data, err = os.ReadFile(path + ".1")
			if tt.backup {
				if err != nil || string(data) != "old content\n" {
					t.Fatalf("backup=%q err=%v", data, err)
				}
				info, _ := os.Stat(path + ".1")
				if info.Mode().Perm() != 0600 || !info.ModTime().Equal(before.ModTime()) {
					t.Fatal("backup metadata lost")
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("expired content archived: %q %v", data, err)
			}
		})
	}
}

func TestLogRotationDuringWritesAndCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-cache.log")
	p := newPrinter(path, 8, 7)
	defer p.Close()
	for _, line := range []string{"one", "two", "three", "four"} {
		p.Info("%s", line)
	}
	data, _ := os.ReadFile(path)
	backup, _ := os.ReadFile(path + ".1")
	if string(data) != "four\n" || string(backup) != "three\n" {
		t.Fatalf("current=%q backup=%q", data, backup)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path+".1", old, old); err != nil {
		t.Fatal(err)
	}
	p.Info("x")
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("expired backup remains")
	}
	p.Close()
	p.Info("after close") // 不再写入已关闭句柄。
}

func TestLogRotationFailureKeepsOriginal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-cache.log")
	if err := os.WriteFile(path, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".1", 0700); err != nil {
		t.Fatal(err)
	}
	p := newPrinter(path, 4, 7)
	defer p.Close()
	p.Info("new")
	data, _ := os.ReadFile(path)
	if string(data) != "old\nnew\n" {
		t.Fatalf("rotation failure lost data: %q", data)
	}
}

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
	if got := strings.Join(next.loadLearned(), "\n"); !strings.Contains(got, u) {
		t.Fatalf("confirmed page was not loaded for exclusion: %q", got)
	}
	if got := slowRequests.Load(); got != 2 {
		t.Fatalf("confirmed page was requested again: got %d requests, want 2", got)
	}
}

func newTestCache(t *testing.T, cfg Config) *PreCache {
	t.Helper()
	cfg.NoLearn = true
	if cfg.Timeout == 0 {
		cfg.Timeout = time.Second
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 1
	}
	pc, err := NewPreCache(cfg, &Printer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pc.transport.CloseIdleConnections)
	return pc
}

// Reproduce the incident at one worker: successful but expensive PHP pages must
// cause a real idle period and eventually stop, even without any HTTP errors.
func TestSerialSlow200CoolsDownAndStops(t *testing.T) {
	var mu sync.Mutex
	var starts, ends []time.Time
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("X-Cache", "MISS")
		_, _ = w.Write([]byte("page"))
		mu.Lock()
		ends = append(ends, time.Now())
		mu.Unlock()
	}))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL, CacheHeader: "X-Cache", Concurrency: 1,
		MaxLatency: 10 * time.Millisecond, Cooldown: 70 * time.Millisecond, MaxSlow: 2})
	pages := make([]Page, 5)
	for i := range pages {
		pages[i] = Page{RawURL: s.URL, URL: s.URL}
	}
	results := pc.run(context.Background(), pages)
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 2 || !pc.aborted() {
		t.Fatalf("sent %d slow 200 requests, aborted=%v; want 2 and stop", len(starts), pc.aborted())
	}
	if gap := starts[1].Sub(ends[0]); gap < 60*time.Millisecond {
		t.Fatalf("no server recovery period between slow 200s: %v", gap)
	}
	stats := summarize(results)
	if stats.Miss != 2 || stats.Skipped != 3 || pc.pressureReason() == "" {
		t.Fatalf("lost successful response or stop reason: %+v / %q", stats, pc.pressureReason())
	}
}

func TestFirstTimeoutStopsWhileServerKeepsWorking(t *testing.T) {
	var pages atomic.Int32
	stillWorking := make(chan struct{})
	finished := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte("healthy cached root"))
		case "/sitemap.xml":
			base := "http://" + r.Host
			_, _ = fmt.Fprintf(w, "<urlset><url><loc>%s/slow</loc></url><url><loc>%s/next</loc></url></urlset>", base, base)
		default:
			pages.Add(1)
			// Model PHP ignoring client disconnect: keep executing after timeout.
			<-stillWorking
			close(finished)
		}
	}))
	defer s.Close()
	defer close(stillWorking)
	report := filepath.Join(t.TempDir(), "report.json")
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", Timeout: 70 * time.Millisecond,
		MaxLatency: time.Second, MaxSlow: 3, Retries: 2, Recheck: time.Millisecond, JSONPath: report})
	code, err := pc.Start(context.Background())
	if err != nil || code != 4 || pages.Load() != 1 {
		t.Fatalf("timeout must stop before next page: code=%d err=%v pages=%d", code, err, pages.Load())
	}
	select {
	case <-finished:
		t.Fatal("test must model work persisting on server after client exit")
	default:
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var rep jsonReport
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.ExitCode != 4 || rep.Completed != 1 || rep.Counts["skipped"] != 1 || rep.StopReason == "" || rep.Recheck != nil {
		t.Fatalf("incomplete pressure stop report: %+v", rep)
	}
}

func TestPressureRestAndSlowStreak(t *testing.T) {
	for _, class := range []Class{ClassHit, ClassMiss, ClassNoHeader, ClassUncacheable, ClassUnclassified} {
		pc := newTestCache(t, Config{Sitemap: "http://example.com", Delay: 10 * time.Millisecond, RestRatio: 2})
		start := time.Now()
		pc.notePressure(Result{Class: class, Elapsed: 50 * time.Millisecond})
		want := 100 * time.Millisecond
		if class == ClassHit {
			want = 10 * time.Millisecond
		}
		if got := pc.pauseUntil.Sub(start); got < want || got > want+50*time.Millisecond {
			t.Fatalf("class %v recovery period=%v want ~%v", class, got, want)
		}
	}
	pc := newTestCache(t, Config{Sitemap: "http://example.com", MaxLatency: time.Second, MaxSlow: 2})
	slow := Result{Class: ClassMiss, Elapsed: 2 * time.Second}
	pc.notePressure(slow)
	pc.notePressure(Result{Class: ClassHit, Elapsed: time.Millisecond})
	pc.notePressure(slow)
	pc.notePressure(Result{Class: ClassSkipped, Timeout: true})
	if pc.aborted() || pc.slowStreak != 1 {
		t.Fatal("fast responses must reset streak; skipped requests must not affect it")
	}
	pc.notePressure(slow)
	if !pc.aborted() {
		t.Fatal("second consecutive slow response did not stop")
	}
}

func TestCooldownCancellationAndRateAfterResume(t *testing.T) {
	pc := newTestCache(t, Config{Sitemap: "http://example.com", Rate: 10})
	pc.pauseUntil = time.Now().Add(80 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pc.waitRequest(ctx); err != context.Canceled {
		t.Fatalf("cooldown cancellation: %v", err)
	}
	var wg sync.WaitGroup
	ready := make(chan time.Time, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if pc.waitRequest(context.Background()) == nil {
				ready <- time.Now()
			}
		}()
	}
	wg.Wait()
	close(ready)
	var prev time.Time
	for now := range ready {
		if now.Before(pc.pauseUntil) {
			t.Fatal("request bypassed global cooldown")
		}
		if !prev.IsZero() && now.Sub(prev) < 80*time.Millisecond {
			t.Fatalf("cooldown resume bypassed QPS: gap %v", now.Sub(prev))
		}
		prev = now
	}
}

func TestPressureDelayAppliesBeforeRetry(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		n := len(times)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL, Delay: 1500 * time.Millisecond, Retries: 1})
	r := pc.probe(context.Background(), Page{RawURL: s.URL, URL: s.URL})
	mu.Lock()
	defer mu.Unlock()
	if r.Status != 200 || len(times) != 2 || times[1].Sub(times[0]) < 1450*time.Millisecond {
		t.Fatalf("retry bypassed post-response rest: result=%+v times=%v", r, times)
	}
}

func TestInFlightRedirectDoesNotSpendTimeoutOnCooldown(t *testing.T) {
	started, proceed := make(chan struct{}), make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			close(started)
			<-proceed
			http.Redirect(w, r, "/page", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("page"))
	}))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL, Timeout: 200 * time.Millisecond,
		MaxLatency: time.Second, Cooldown: time.Second})
	result := make(chan Result, 1)
	go func() {
		r, _ := pc.once(context.Background(), Page{RawURL: s.URL + "/redirect", URL: s.URL + "/redirect"}, http.MethodGet)
		result <- r
	}()
	<-started
	// A different worker completes a slow request while this one is in flight.
	pc.notePressure(Result{Class: ClassMiss, Elapsed: time.Second})
	close(proceed)
	r := <-result
	if r.Status != 200 || r.Timeout || r.Elapsed >= pc.cfg.Timeout {
		t.Fatalf("local cooldown caused a false upstream timeout on redirect: %+v", r)
	}
}
func TestResponseBodyTimeout(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", CacheHeader: "X-Cache", Timeout: 50 * time.Millisecond})
	r, _ := pc.once(context.Background(), Page{RawURL: s.URL, URL: s.URL}, http.MethodGet)
	if !r.Timeout || r.Class != ClassException {
		t.Fatalf("body timed out but class=%v status=%d timeout=%v err=%q", r.Class, r.Status, r.Timeout, r.Err)
	}
}
func TestIncompleteSitemapFailsRun(t *testing.T) {
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sitemap.xml":
			fmt.Fprintf(w, "<sitemapindex><sitemap><loc>%s/good.xml</loc></sitemap><sitemap><loc>%s/bad.xml</loc></sitemap></sitemapindex>", s.URL, s.URL)
		case "/good.xml":
			fmt.Fprintf(w, "<urlset><url><loc>%s/page</loc></url></urlset>", s.URL)
		case "/bad.xml":
			w.WriteHeader(503)
		default:
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(200)
		}
	}))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", CacheHeader: "X-Cache"})
	code, err := pc.Start(context.Background())
	if code == 0 && err == nil {
		t.Fatal("one of two child sitemaps failed with 503 but Start returned code=0, err=nil")
	}
}
func TestRetriesRespectRate(t *testing.T) {
	var mu sync.Mutex
	var stamps []time.Time
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		n := len(stamps)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(200)
		}
	}))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", Rate: 1, Retries: 1})
	pc.run(context.Background(), []Page{{RawURL: s.URL, URL: s.URL}})
	mu.Lock()
	defer mu.Unlock()
	gap := stamps[1].Sub(stamps[0])
	if gap < 900*time.Millisecond {
		t.Fatalf("rate=1 but retry request gap=%v", gap)
	}
}
func TestRecheckIncludesFailures(t *testing.T) {
	var a, b atomic.Int32
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sitemap.xml":
			fmt.Fprintf(w, "<urlset><url><loc>%s/a</loc></url><url><loc>%s/b</loc></url></urlset>", s.URL, s.URL)
		case "/a":
			if a.Add(1) == 1 {
				w.Header().Set("X-Cache", "MISS")
			} else {
				w.Header().Set("X-Cache", "HIT")
			}
		case "/b":
			if b.Add(1) == 1 {
				w.Header().Set("X-Cache", "MISS")
			} else {
				w.WriteHeader(503)
			}
		default:
			w.WriteHeader(200)
		}
	}))
	defer s.Close()
	report := filepath.Join(t.TempDir(), "report.json")
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", CacheHeader: "X-Cache", Recheck: time.Millisecond, JSONPath: report, MaxErrRate: 1})
	code, err := pc.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var rep jsonReport
	if err = json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Recheck.HitRate != 50 || code == 0 {
		t.Fatalf("recheck: 1 HIT + 1 HTTP503, report=%+v, first-pass error_rate=%v exit=%d", rep.Recheck, rep.ErrorRate, code)
	}
}
func TestIncrementalNoChanges(t *testing.T) {
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sitemap.xml" {
			fmt.Fprintf(w, "<urlset><url><loc>%s/old</loc><lastmod>2020-01-01</lastmod></url></urlset>", s.URL)
		}
	}))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", LastMod: time.Hour})
	code, err := pc.Start(context.Background())
	if code != 0 || err != nil {
		t.Fatalf("valid sitemap, zero recently changed pages: exit=%d err=%v", code, err)
	}
}
func TestLearnedURLMatchesExactly(t *testing.T) {
	var n atomic.Int32
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sitemap.xml" {
			fmt.Fprintf(w, "<urlset><url><loc>%s/post/1</loc></url><url><loc>%s/post/10</loc></url></urlset>", s.URL, s.URL)
		} else if strings.HasPrefix(r.URL.Path, "/post/") {
			n.Add(1)
		}
	}))
	defer s.Close()
	path := filepath.Join(t.TempDir(), "slow.txt")
	if err := os.WriteFile(path, []byte(s.URL+"/post/1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", LearnPath: path})
	pc.cfg.NoLearn = false
	code, err := pc.Start(context.Background())
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("learned /post/1 also excludes /post/10, executed requests=%d", got)
	}
}
func TestReportWriteFailureIsFatal(t *testing.T) {
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sitemap.xml" {
			fmt.Fprintf(w, "<urlset><url><loc>%s/page</loc></url></urlset>", s.URL)
		}
	}))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", JSONPath: filepath.Join(t.TempDir(), "missing", "report.json")})
	code, err := pc.Start(context.Background())
	if code == 0 && err == nil {
		t.Fatal("required JSON report cannot be written but exit=0, err=nil")
	}
}
func TestRetryAfterHonored(t *testing.T) {
	got := backoff(1, 120*time.Second)
	if got < 120*time.Second {
		t.Fatalf("Retry-After=120s shortened to %v", got)
	}
}
func TestHealthRejects429(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "120"); w.WriteHeader(429) }))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml"})
	if err := pc.healthCheck(context.Background()); err == nil {
		t.Fatal("HTTP429 is marked healthy")
	}
}
func TestServerErrorRate(t *testing.T) {
	stats := summarize([]Result{{Status: 403, Class: ClassHTTPError}, {Status: 200, Class: ClassHit}})
	if rate := stats.errRate(); rate != 0 {
		t.Fatalf("documented server-unavailable metric counts HTTP403, rate=%v%%", rate)
	}
}
func TestCircuitCountsEachAttempt(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) }))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", MaxFail: 1, Retries: 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pc.cancel = cancel
	pc.run(ctx, []Page{{RawURL: s.URL, URL: s.URL}})
	if n := requests.Load(); n != 1 {
		t.Fatalf("max-fail=1 retry=2, requests before trip=%d", n)
	}
}

func TestSlowThresholdAndTopK(t *testing.T) {
	pc := newTestCache(t, Config{Sitemap: "https://example.com/sitemap.xml"})
	if pc.cfg.SlowThreshold != 2*time.Second {
		t.Fatalf("default threshold: %v", pc.cfg.SlowThreshold)
	}
	results := []Result{
		{Page: Page{RawURL: "boundary"}, Class: ClassHit, Elapsed: 2 * time.Second},
		{Page: Page{RawURL: "normal"}, Class: ClassHit, Elapsed: 600 * time.Millisecond},
		{Page: Page{RawURL: "timeout"}, Class: ClassException, Timeout: true, Elapsed: 100 * time.Millisecond},
		{Page: Page{RawURL: "skipped"}, Class: ClassSkipped, Elapsed: time.Hour},
	}
	for i := 3; i < 20; i++ {
		results = append(results, Result{Page: Page{RawURL: fmt.Sprint(i)}, Class: ClassHit, Elapsed: time.Duration(i) * time.Second})
	}
	top := pc.slowest(results, 5)
	if len(top) != 5 || top[0].Elapsed != 19*time.Second || top[4].Elapsed != 15*time.Second {
		t.Fatalf("unexpected top 5: %+v", top)
	}
	if results[0].Page.RawURL != "boundary" {
		t.Fatal("input was reordered")
	}
	pc.cfg.SlowThreshold = 30 * time.Second
	top = pc.slowest(results, 10)
	if len(top) != 1 || top[0].Page.RawURL != "timeout" {
		t.Fatalf("timeouts must remain visible above threshold: %+v", top)
	}
	pc.cfg.SlowThreshold = 500 * time.Millisecond
	path := filepath.Join(t.TempDir(), "report.json")
	if err := pc.writeJSON(path, results[:2], summarize(results[:2]), time.Second, nil, 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report jsonReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.SlowThresholdSec != 0.5 || len(report.Slowest) != 2 {
		t.Fatalf("JSON ignored threshold: %+v", report)
	}
}

func TestLargeBodyDrainedAndConnectionReused(t *testing.T) {
	var connections atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cache", "HIT")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1024*1024))
	}))
	s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	s.Start()
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", CacheHeader: "X-Cache"})
	for i := 0; i < 2; i++ {
		r, _ := pc.once(context.Background(), Page{RawURL: s.URL, URL: s.URL}, http.MethodGet)
		if r.Class != ClassHit {
			t.Fatalf("large response failed: %+v", r)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("connection not reused for >512KiB response: %d connections", got)
	}
}

func TestSitemapGzipVariants(t *testing.T) {
	for _, mode := range []string{"file", "encoding", "transparent", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			data := []byte("<urlset><url><loc>https://example.com/page</loc></url></urlset>")
			var compressed bytes.Buffer
			gz := gzip.NewWriter(&compressed)
			_, _ = gz.Write(data)
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "transparent" {
					_, _ = w.Write(data)
					return
				}
				if mode == "encoding" {
					w.Header().Set("Content-Encoding", "gzip")
				}
				body := compressed.Bytes()
				if mode == "corrupt" {
					body = body[:len(body)-5]
				}
				_, _ = w.Write(body)
			}))
			defer s.Close()
			target := s.URL + "/sitemap.xml.gz?token=1"
			pc := newTestCache(t, Config{Sitemap: target})
			got, err := pc.getBytes(context.Background(), target)
			if mode == "corrupt" {
				if err == nil {
					t.Fatal("corrupt gzip accepted")
				}
				return
			}
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("getBytes: %q, %v", got, err)
			}
		})
	}
}

func TestRedirectsRespectRate(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		if r.URL.Path == "/from" {
			http.Redirect(w, r, "/to", http.StatusFound)
		}
	}))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", Rate: 5})
	r, _ := pc.once(context.Background(), Page{RawURL: s.URL + "/from", URL: s.URL + "/from"}, http.MethodGet)
	if r.Status != 200 {
		t.Fatalf("redirect failed: %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) != 2 || times[1].Sub(times[0]) < 180*time.Millisecond {
		t.Fatalf("redirect bypassed limiter: %v", times)
	}
}

func TestCancellationIsSkippedAndReported(t *testing.T) {
	started := make(chan struct{})
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sitemap.xml":
			fmt.Fprintf(w, "<urlset><url><loc>%s/slow</loc></url></urlset>", s.URL)
		case "/slow":
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
		}
	}))
	defer s.Close()
	reportPath := filepath.Join(t.TempDir(), "report.json")
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", JSONPath: reportPath, MaxFail: 1, MaxErrRate: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() { code, _ := pc.Start(ctx); done <- code }()
	select {
	case <-started:
		cancel()
	case <-ctx.Done():
		t.Fatal("request did not start")
	}
	if code := <-done; code != 130 {
		t.Fatalf("cancellation exit=%d, want 130", code)
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report jsonReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Counts["exception"] != 0 || report.Counts["skipped"] != 1 || report.Completed != 0 || report.ExitCode != 130 {
		t.Fatalf("cancelled request reported as failure: %+v", report)
	}
}

func TestNoMissDoesNotWaitForRecheck(t *testing.T) {
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sitemap.xml" {
			fmt.Fprintf(w, "<urlset><url><loc>%s/page</loc></url></urlset>", s.URL)
		} else {
			w.Header().Set("X-Cache", "HIT")
		}
	}))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", CacheHeader: "X-Cache", Recheck: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if code, err := pc.Start(ctx); code != 0 || err != nil {
		t.Fatalf("unnecessary recheck wait: code=%d err=%v", code, err)
	}
}

func TestInvalidNumericConfiguration(t *testing.T) {
	for _, cfg := range []Config{
		{Rate: math.NaN()}, {Rate: math.Inf(1)}, {Rate: -1}, {Rate: 1e-100},
		{SlowRatio: math.NaN()}, {MaxErrRate: 101}, {SlowThreshold: -time.Second},
	} {
		cfg.Sitemap = "https://example.com/sitemap.xml"
		if _, err := NewPreCache(cfg, &Printer{}); err == nil {
			t.Fatalf("invalid config accepted: %+v", cfg)
		}
	}
	for _, n := range []int{7, 63, 10000} {
		if d := backoff(n, 0); d < 10*time.Second || d > 15*time.Second {
			t.Fatalf("overflowed backoff for attempt %d: %v", n, d)
		}
	}
	if got := parseRetryAfter("9223372036854775807"); got <= 0 {
		t.Fatalf("Retry-After overflow: %v", got)
	}
}

func TestIncrementalKeepsUnknownDatesWithWarning(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<urlset><url><loc>https://example.com/old</loc><lastmod>2020-01-01</lastmod></url><url><loc>https://example.com/unknown</loc></url></urlset>`)
	}))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", LastMod: time.Hour})
	logFile, err := os.CreateTemp(t.TempDir(), "incremental-log-*")
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	pc.out.file = logFile
	pages, err := pc.loadPages(context.Background())
	if err != nil || len(pages) != 1 || pages[0].RawURL != "https://example.com/unknown" {
		t.Fatalf("incremental selection: %v, %v", pages, err)
	}
	logData, err := os.ReadFile(logFile.Name())
	if err != nil || !strings.Contains(string(logData), "增量可能退化为全量") {
		t.Fatalf("missing lastmod warning: %q, %v", logData, err)
	}
}

func TestAtomicReportPreservesPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte("old"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, []byte(`{"completed":1}`)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("report permissions changed: %v, %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"completed":1}` {
		t.Fatalf("report replacement failed: %q, %v", data, err)
	}
}

func TestErrorRateExcludesSkippedAndNonServerErrors(t *testing.T) {
	s := summarize([]Result{
		{Status: 451, Class: ClassHTTPError}, {Status: 403, Class: ClassHTTPError},
		{Status: 404, Class: ClassNotFound}, {Status: 429, Class: ClassHTTPError},
		{Status: 503, Class: ClassHTTPError}, {Class: ClassException, Timeout: true},
		{Class: ClassSkipped},
	})
	if got := s.errRate(); got != 50 {
		t.Fatalf("server error rate=%v, want 3/6=50%%", got)
	}
}

func TestConcurrencyAndAdaptiveCap(t *testing.T) {
	var active, peak atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}))
	defer s.Close()
	pc := newTestCache(t, Config{Sitemap: s.URL + "/sitemap.xml", Concurrency: 3})
	pages := make([]Page, 20)
	for i := range pages {
		pages[i] = Page{RawURL: s.URL, URL: s.URL}
	}
	results := pc.run(context.Background(), pages)
	if peak.Load() > 3 || summarize(results).Skipped != 0 {
		t.Fatalf("worker cap failed: peak=%d stats=%+v", peak.Load(), summarize(results))
	}
	th := newThrottle(4, 2, &Printer{})
	for i := 0; i < 10; i++ {
		th.observe(50 * time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		th.observe(300 * time.Millisecond)
	}
	if th.limitNow() != 2 {
		t.Fatalf("adaptive cap not reduced: %d", th.limitNow())
	}
}
