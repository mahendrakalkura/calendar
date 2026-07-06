package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

type account struct {
	AuthUser   string `json:"authuser"`
	CalendarID string `json:"calendar_id"`
	Name       string `json:"name"`
	Priority   int    `json:"priority"`
	TokenFile  string `json:"token_file"`
}

type accountsConfig struct {
	Accounts []account `json:"accounts"`
}

type cacheEntry struct {
	Entries   []eventEntry `json:"entries"`
	FetchedAt time.Time    `json:"fetched_at"`
}

type eventEntry struct {
	Account       string
	AllDay        bool
	Calendar      string
	CalendarEmail string
	DedupeKey     string
	End           time.Time
	Link          string
	Priority      int
	Start         time.Time
	StartText     string
	Summary       string
}

type savingTokenSource struct {
	current *oauth2.Token
	mu      sync.Mutex
	path    string
	source  oauth2.TokenSource
}

const cachePath = "events.json"
const cacheTTL = 60 * time.Minute

var authMu sync.Mutex

var meetingLinkRegex = regexp.MustCompile(`https?://(?:meet\.google\.com|[\w.-]*zoom\.us|teams\.microsoft\.com|teams\.live\.com|[\w.-]*webex\.com|meet\.jit\.si|whereby\.com|[\w.-]*\.gotomeeting\.com|[\w.-]*\.bluejeans\.com)/[^\s<>"')]+`)

func main() {
	ctx := context.Background()

	tzAbbr := flag.String("tz", "IST", "Timezone abbreviation (IST, PST, EST, CST, MST, GMT, UTC, etc.)")
	force := flag.Bool("force", false, "Bypass cache and refetch")
	flag.Parse()

	ianaName, ok := resolveTimezone(strings.ToUpper(*tzAbbr))
	if !ok {
		log.Fatalf("unknown timezone: %q", *tzAbbr)
	}

	loc, err := time.LoadLocation(ianaName)
	if err != nil {
		log.Fatalf("load timezone %s: %v", ianaName, err)
	}

	cfg, err := loadAccounts("accounts.json")
	if err != nil {
		log.Fatalf("load accounts.json: %v", err)
	}
	if len(cfg.Accounts) == 0 {
		log.Fatal("accounts.json has no accounts")
	}

	b, err := os.ReadFile("credentials.json")
	if err != nil {
		log.Fatalf("read credentials.json: %v", err)
	}
	oauthCfg, err := google.ConfigFromJSON(b, calendar.CalendarReadonlyScope)
	if err != nil {
		log.Fatalf("parse credentials.json: %v", err)
	}

	rangeStart, rangeEnd := eventRange(loc)

	for _, acct := range cfg.Accounts {
		if strings.TrimSpace(acct.Name) == "" {
			log.Fatal("account name cannot be empty")
		}
		if strings.TrimSpace(acct.TokenFile) == "" {
			log.Fatal("token_file cannot be empty")
		}
		if strings.TrimSpace(acct.CalendarID) == "" {
			log.Fatal("calendar_id cannot be empty")
		}
	}

	cache := loadCache()

	results := make([][]eventEntry, len(cfg.Accounts))
	errs := make([]error, len(cfg.Accounts))
	var wg sync.WaitGroup
	for i, acct := range cfg.Accounts {
		wg.Add(1)
		go func(i int, acct account) {
			defer wg.Done()
			results[i], errs[i] = fetchOrCached(ctx, oauthCfg, acct, rangeStart, rangeEnd, loc, *force, cache)
		}(i, acct)
	}
	wg.Wait()

	var all []eventEntry
	for i, acct := range cfg.Accounts {
		if errs[i] != nil {
			log.Fatalf("fetch (%s): %v", acct.Name, errs[i])
		}
		all = append(all, results[i]...)
	}

	saveCache(cache)

	all = dedupeEntries(all)
	sort.Slice(all, func(i, j int) bool {
		if all[i].Start.Equal(all[j].Start) {
			return all[i].Summary < all[j].Summary
		}
		return all[i].Start.Before(all[j].Start)
	})

	if len(all) == 0 {
		fmt.Println("No events found.")
		return
	}

	printTable(all, loc, *tzAbbr)
}

type concurrentCache struct {
	data map[string]cacheEntry
	mu   sync.Mutex
}

func loadCache() *concurrentCache {
	data := map[string]cacheEntry{}
	b, err := os.ReadFile(cachePath)
	if err == nil {
		e := json.Unmarshal(b, &data)
		if e != nil {
			data = map[string]cacheEntry{}
		}
	}
	return &concurrentCache{data: data}
}

func saveCache(c *concurrentCache) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, err := json.MarshalIndent(c.data, "", "  ")
	if err != nil {
		return
	}
	err = os.WriteFile(cachePath, out, 0o644)
	if err != nil {
		log.Printf("write cache: %v", err)
	}
}

func fetchOrCached(ctx context.Context, oauthCfg *oauth2.Config, acct account, rangeStart, rangeEnd time.Time, loc *time.Location, force bool, cache *concurrentCache) ([]eventEntry, error) {
	key := fmt.Sprintf("%s|%s", acct.CalendarID, rangeStart.Format("2006-01-02"))
	if !force {
		cache.mu.Lock()
		entry, ok := cache.data[key]
		cache.mu.Unlock()
		if ok && time.Since(entry.FetchedAt) < cacheTTL {
			return entry.Entries, nil
		}
	}
	entries, err := fetchAccountEvents(ctx, oauthCfg, acct, rangeStart, rangeEnd, loc)
	if err != nil {
		return nil, err
	}
	cache.mu.Lock()
	cache.data[key] = cacheEntry{FetchedAt: time.Now(), Entries: entries}
	cache.mu.Unlock()
	return entries, nil
}

func eventRange(loc *time.Location) (time.Time, time.Time) {
	now := time.Now().In(loc)
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	rangeStart := startOfDay.AddDate(0, 0, -3)
	rangeEnd := startOfDay.AddDate(0, 0, 8)
	return rangeStart, rangeEnd
}

func dedupeEntries(entries []eventEntry) []eventEntry {
	seen := make(map[string]eventEntry, len(entries))
	result := make([]eventEntry, 0, len(entries))
	for _, e := range entries {
		key := e.DedupeKey
		if key == "" {
			key = fmt.Sprintf("%s|%s", e.Summary, e.Start.UTC().Format(time.RFC3339))
		}
		existing, ok := seen[key]
		if ok {
			if e.Priority < existing.Priority {
				seen[key] = e
			}
			continue
		}
		seen[key] = e
	}
	for _, e := range seen {
		result = append(result, e)
	}
	return result
}

func extractLink(item *calendar.Event) string {
	if strings.TrimSpace(item.HangoutLink) != "" {
		return item.HangoutLink
	}
	if item.ConferenceData != nil {
		for _, ep := range item.ConferenceData.EntryPoints {
			if strings.TrimSpace(ep.Uri) != "" {
				return ep.Uri
			}
		}
	}
	for _, field := range []string{item.Location, item.Description} {
		match := meetingLinkRegex.FindString(field)
		if match != "" {
			return match
		}
	}
	return ""
}

func fetchAccountEvents(ctx context.Context, oauthCfg *oauth2.Config, acct account, rangeStart, rangeEnd time.Time, loc *time.Location) ([]eventEntry, error) {
	for attempt := 0; attempt < 2; attempt++ {
		client, err := getClient(ctx, oauthCfg, acct.TokenFile, acct.Name)
		if err != nil {
			return nil, err
		}

		srv, err := calendar.NewService(ctx, option.WithHTTPClient(client))
		if err != nil {
			return nil, err
		}

		calendarEmail := acct.CalendarID
		cal, err := srv.Calendars.Get(acct.CalendarID).Do()
		if err == nil && strings.TrimSpace(cal.Id) != "" {
			calendarEmail = cal.Id
		}

		call := srv.Events.List(acct.CalendarID).
			ShowDeleted(false).
			SingleEvents(true).
			OrderBy("startTime").
			TimeMin(rangeStart.Format(time.RFC3339)).
			TimeMax(rangeEnd.Format(time.RFC3339)).
			TimeZone(loc.String())

		var entries []eventEntry
		for {
			events, err := call.Do()
			if err == nil {
				for _, item := range events.Items {
					entry, ok := toEntry(item, acct, calendarEmail)
					if !ok {
						continue
					}
					entries = append(entries, entry)
				}
				if events.NextPageToken == "" {
					return entries, nil
				}
				call = call.PageToken(events.NextPageToken)
				continue
			}

			if !isInvalidGrantError(err) || attempt == 1 {
				return nil, err
			}
			break
		}

		fmt.Printf("Token for %s was rejected by Google; re-authorizing...\n", acct.Name)
		tok, authErr := getTokenFromWeb(oauthCfg, acct.Name)
		if authErr != nil {
			return nil, fmt.Errorf("re-authorization failed: %w", authErr)
		}
		err = saveToken(acct.TokenFile, tok)
		if err != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf("re-authentication failed for %s", acct.Name)
}

func formatRow(e eventEntry, loc *time.Location) (summary, from, to, dur string) {
	tzTime := e.Start.In(loc)
	tzEnd := e.End.In(loc)

	if e.AllDay {
		from = "All day"
		to = "All day"
	} else {
		from = formatTime(tzTime)
		to = formatTime(tzEnd)
	}

	durationMinutes := int(e.End.Sub(e.Start).Minutes())
	if durationMinutes < 0 {
		durationMinutes = 0
	}

	return e.Summary, from, to, fmt.Sprintf("%d", durationMinutes)
}

func formatTime(t time.Time) string {
	h := t.Hour() % 12
	if h == 0 {
		h = 12
	}
	m := t.Minute()
	ampm := "am"
	if t.Hour() >= 12 {
		ampm = "pm"
	}
	return fmt.Sprintf("%2d:%02d%s", h, m, ampm)
}

func getClient(ctx context.Context, config *oauth2.Config, tokenFile, label string) (*http.Client, error) {
	tok, err := tokenFromFile(tokenFile)
	if err != nil {
		tok, err = getTokenFromWeb(config, label)
		if err != nil {
			return nil, err
		}
		err = saveToken(tokenFile, tok)
		if err != nil {
			return nil, err
		}
	}
	source := &savingTokenSource{
		source:  config.TokenSource(ctx, tok),
		path:    tokenFile,
		current: tok,
	}
	return oauth2.NewClient(ctx, source), nil
}

func getTokenFromWeb(config *oauth2.Config, label string) (*oauth2.Token, error) {
	authMu.Lock()
	defer authMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer listener.Close()

	localConfig := *config
	localConfig.RedirectURL = fmt.Sprintf("http://%s/oauth2callback", listener.Addr().String())

	state, err := randomState()
	if err != nil {
		return nil, err
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	server := &http.Server{Handler: mux}
	mux.HandleFunc("/oauth2callback", func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("state")
		if got != state {
			http.Error(w, "Invalid OAuth state.", http.StatusBadRequest)
			select {
			case errCh <- errors.New("invalid OAuth state"):
			default:
			}
			return
		}
		oauthErr := r.URL.Query().Get("error")
		if oauthErr != "" {
			http.Error(w, "OAuth authorization failed.", http.StatusBadRequest)
			select {
			case errCh <- fmt.Errorf("oauth authorization failed: %s", oauthErr):
			default:
			}
			return
		}
		code := strings.TrimSpace(r.URL.Query().Get("code"))
		if code == "" {
			http.Error(w, "Missing OAuth authorization code.", http.StatusBadRequest)
			select {
			case errCh <- errors.New("missing OAuth authorization code"):
			default:
			}
			return
		}

		fmt.Fprintln(w, "Authorization received. You can close this browser tab and return to the terminal.")
		select {
		case codeCh <- code:
		default:
		}
	})
	go func() {
		serveErr := server.Serve(listener)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	}()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutdownCancel()
		e := server.Shutdown(shutdownCtx)
		if e != nil {
			log.Printf("server shutdown: %v", e)
		}
	}()

	authURL := localConfig.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
	fmt.Printf("Opening browser for authorization (%s):\n%v\n", label, authURL)
	openErr := openBrowser(authURL)
	if openErr != nil {
		fmt.Printf("Could not open browser automatically: %v\n", openErr)
		fmt.Println("Open the authorization URL manually; the code will be captured after Google redirects back.")
	}

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tok, err := localConfig.Exchange(context.Background(), code)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(tok.RefreshToken) == "" {
		return nil, errors.New("authorization response did not include a refresh_token")
	}
	return tok, nil
}

func isInvalidGrantError(err error) bool {
	if err == nil {
		return false
	}
	if strings.Contains(strings.ToLower(err.Error()), "invalid_grant") {
		return true
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		for _, item := range gerr.Errors {
			if strings.EqualFold(item.Reason, "invalid_grant") {
				return true
			}
		}
	}
	return false
}

func loadAccounts(path string) (accountsConfig, error) {
	var cfg accountsConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(b, &cfg)
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func printTable(entries []eventEntry, loc *time.Location, tzAbbr string) {
	now := time.Now().In(loc)

	nextIdx := -1
	for i, e := range entries {
		if e.End.In(loc).After(now) {
			nextIdx = i
			break
		}
	}

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	style := table.StyleLight
	style.Color.Header = text.Colors{text.Bold}
	t.SetStyle(style)
	_ = strings.ToUpper(tzAbbr)
	t.AppendHeader(table.Row{"ACCOUNT", "EVENT", "FROM", "TO", "DURATION", "LINK"})
	lastDate := ""
	for i, e := range entries {
		tzTime := e.Start.In(loc)
		if e.AllDay {
			tzTime = e.Start.UTC()
		}
		dateKey := tzTime.Format("2006-01-02")
		if dateKey != lastDate {
			if lastDate != "" {
				t.AppendSeparator()
			}
			header := text.Colors{text.Bold}.Sprintf("%s %s", dateKey, tzTime.Format("Mon"))
			t.AppendRow(table.Row{header, "", "", "", "", ""})
			t.AppendSeparator()
		}
		lastDate = dateKey

		summary, from, to, dur := formatRow(e, loc)

		past := e.End.In(loc).Before(now)
		var wrap func(string) string
		if i == nextIdx {
			wrap = func(s string) string { return text.Colors{text.FgHiRed, text.Bold}.Sprint(s) }
		} else if past {
			wrap = func(s string) string { return text.Colors{text.FgHiBlack}.Sprint(s) }
		} else {
			wrap = func(s string) string { return text.Colors{text.Bold}.Sprint(s) }
		}

		t.AppendRow(table.Row{
			wrap(e.CalendarEmail),
			wrap(summary),
			wrap(from),
			wrap(to),
			wrap(dur),
			wrap(e.Link),
		})
	}
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 3, Align: text.AlignRight, AlignHeader: text.AlignRight},
		{Number: 4, Align: text.AlignRight, AlignHeader: text.AlignRight},
		{Number: 5, Align: text.AlignRight, AlignHeader: text.AlignRight},
	})
	t.Render()
}

func randomState() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func resolveTimezone(abbr string) (string, bool) {
	m := map[string]string{
		"ACDT": "Australia/Adelaide",
		"ACST": "Australia/Darwin",
		"AEDT": "Australia/Sydney",
		"AEST": "Australia/Brisbane",
		"AWST": "Australia/Perth",
		"BST":  "Europe/London",
		"CEST": "Europe/Berlin",
		"CET":  "Europe/Berlin",
		"CST":  "America/Chicago",
		"EDT":  "America/New_York",
		"EEST": "Europe/Helsinki",
		"EET":  "Europe/Helsinki",
		"EST":  "America/New_York",
		"GMT":  "Europe/London",
		"IST":  "Asia/Kolkata",
		"JST":  "Asia/Tokyo",
		"KST":  "Asia/Seoul",
		"MDT":  "America/Denver",
		"MST":  "America/Denver",
		"NZDT": "Pacific/Auckland",
		"NZST": "Pacific/Auckland",
		"PDT":  "America/Los_Angeles",
		"PST":  "America/Los_Angeles",
		"UTC":  "Etc/UTC",
	}
	iana, ok := m[abbr]
	return iana, ok
}

func saveToken(path string, token *oauth2.Token) error {
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(token)
	if err != nil {
		e := f.Close()
		if e != nil {
			log.Printf("token close: %v", e)
		}
		return err
	}
	return f.Close()
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := s.source.Token()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tok = tokenWithRefreshToken(tok, s.current)
	if tokensEqual(s.current, tok) {
		return tok, nil
	}
	err = saveToken(s.path, tok)
	if err != nil {
		return nil, err
	}
	s.current = tok
	return tok, nil
}

func toEntry(item *calendar.Event, acct account, calendarEmail string) (eventEntry, bool) {
	if item.Start == nil || item.End == nil {
		return eventEntry{}, false
	}

	var start, end time.Time
	allDay := false
	startText := ""

	if item.Start.DateTime != "" {
		var err error
		start, err = time.Parse(time.RFC3339, item.Start.DateTime)
		if err != nil {
			return eventEntry{}, false
		}
		end, err = time.Parse(time.RFC3339, item.End.DateTime)
		if err != nil {
			return eventEntry{}, false
		}
	} else if item.Start.Date != "" {
		var err error
		start, err = time.Parse("2006-01-02", item.Start.Date)
		if err != nil {
			return eventEntry{}, false
		}
		end, err = time.Parse("2006-01-02", item.End.Date)
		if err != nil {
			return eventEntry{}, false
		}
		allDay = true
		startText = start.Format("2006-01-02")
	}

	if start.IsZero() {
		return eventEntry{}, false
	}

	summary := item.Summary
	if strings.TrimSpace(summary) == "" {
		summary = "(no title)"
	}

	link := extractLink(item)
	if link != "" && strings.Contains(link, "meet.google.com") {
		link = withAuthUser(link, acct.AuthUser)
	}

	entry := eventEntry{
		Account:       acct.Name,
		AllDay:        allDay,
		Calendar:      acct.CalendarID,
		CalendarEmail: calendarEmail,
		DedupeKey:     fmt.Sprintf("%s|%s", summary, start.UTC().Format(time.RFC3339)),
		End:           end.UTC(),
		Link:          link,
		Priority:      acct.Priority,
		Start:         start.UTC(),
		StartText:     startText,
		Summary:       summary,
	}
	return entry, true
}

func tokenFromFile(file string) (tok *oauth2.Token, err error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer func() {
		cerr := f.Close()
		if cerr != nil && err == nil {
			err = cerr
		}
	}()

	var token oauth2.Token
	err = json.NewDecoder(f).Decode(&token)
	if err != nil {
		return nil, err
	}
	return &token, nil
}

func tokenWithRefreshToken(tok, previous *oauth2.Token) *oauth2.Token {
	if tok == nil || tok.RefreshToken != "" || previous == nil || previous.RefreshToken == "" {
		return tok
	}
	merged := *tok
	merged.RefreshToken = previous.RefreshToken
	return &merged
}

func tokensEqual(a, b *oauth2.Token) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.AccessToken == b.AccessToken &&
		a.RefreshToken == b.RefreshToken &&
		a.TokenType == b.TokenType &&
		a.Expiry.Equal(b.Expiry)
}

func withAuthUser(link, authUser string) string {
	authUser = strings.TrimSpace(authUser)
	if authUser == "" {
		return link
	}
	sep := "?"
	if strings.Contains(link, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%sauthuser=%s", link, sep, authUser)
}
