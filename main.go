// Command profilegen refreshes the generated assets on the profile README.
//
// It queries the GitHub GraphQL API for account-wide activity, then:
//   - rewrites the <tspan> placeholders in dark_mode.svg / light_mode.svg
//   - renders stats_dark.svg / stats_light.svg from scratch
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	startDate = "2024-11-01"
	login     = "Astraxx04"
	endpoint  = "https://api.github.com/graphql"
)

// terminalFiles carry <tspan id="..."> placeholders that get patched in place.
var terminalFiles = []string{"dark_mode.svg", "light_mode.svg"}

// excludedLanguages are dropped from the language bar. Notebooks embed their
// own output, so GitHub counts megabytes of base64 images as source bytes and
// they drown out everything else.
var excludedLanguages = map[string]bool{
	"Jupyter Notebook": true,
}

// query pulls user-level stats only — nothing scoped to a single repository.
// The contributionsCollection fields cover the trailing 12 months, which is
// the same window GitHub uses for the contribution graph.
const query = `query($login:String!){
  user(login:$login){
    followers{ totalCount }
    contributionsCollection{
      totalCommitContributions
      totalPullRequestContributions
      totalPullRequestReviewContributions
      totalIssueContributions
      contributionCalendar{
        totalContributions
        weeks{ contributionDays{ contributionCount date } }
      }
    }
    repositories(first:100, ownerAffiliations:OWNER, isFork:false, privacy:PUBLIC){
      totalCount
      nodes{
        stargazerCount
        languages(first:8, orderBy:{field:SIZE, direction:DESC}){
          edges{ size node{ name } }
        }
      }
    }
  }
}`

type gqlResp struct {
	Data struct {
		User struct {
			Followers struct {
				TotalCount int `json:"totalCount"`
			} `json:"followers"`
			ContributionsCollection struct {
				TotalCommitContributions            int `json:"totalCommitContributions"`
				TotalPullRequestContributions       int `json:"totalPullRequestContributions"`
				TotalPullRequestReviewContributions int `json:"totalPullRequestReviewContributions"`
				TotalIssueContributions             int `json:"totalIssueContributions"`
				ContributionCalendar                struct {
					TotalContributions int `json:"totalContributions"`
					Weeks              []struct {
						ContributionDays []struct {
							ContributionCount int    `json:"contributionCount"`
							Date              string `json:"date"`
						} `json:"contributionDays"`
					} `json:"weeks"`
				} `json:"contributionCalendar"`
			} `json:"contributionsCollection"`
			Repositories struct {
				TotalCount int `json:"totalCount"`
				Nodes      []struct {
					StargazerCount int `json:"stargazerCount"`
					Languages      struct {
						Edges []struct {
							Size int `json:"size"`
							Node struct {
								Name string `json:"name"`
							} `json:"node"`
						} `json:"edges"`
					} `json:"languages"`
				} `json:"nodes"`
			} `json:"repositories"`
		} `json:"user"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type day struct {
	count int
	date  time.Time
}

type language struct {
	name    string
	percent float64
}

// stats holds every value rendered onto the stats card.
type stats struct {
	contributions int
	commits       int
	pullRequests  int
	reviews       int
	issues        int
	repos         int
	stars         int
	followers     int
	weeks         [][]day
	maxDay        int
	languages     []language
}

func fetchStats(ctx context.Context, token string) (stats, error) {
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]string{"login": login},
	})
	if err != nil {
		return stats{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return stats{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return stats{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return stats{}, fmt.Errorf("github graphql: unexpected status %s", resp.Status)
	}

	var out gqlResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return stats{}, err
	}
	if len(out.Errors) > 0 {
		return stats{}, fmt.Errorf("github graphql: %s", out.Errors[0].Message)
	}

	u := out.Data.User
	c := u.ContributionsCollection

	s := stats{
		contributions: c.ContributionCalendar.TotalContributions,
		commits:       c.TotalCommitContributions,
		pullRequests:  c.TotalPullRequestContributions,
		reviews:       c.TotalPullRequestReviewContributions,
		issues:        c.TotalIssueContributions,
		repos:         u.Repositories.TotalCount,
		followers:     u.Followers.TotalCount,
	}

	for _, w := range c.ContributionCalendar.Weeks {
		week := make([]day, 0, len(w.ContributionDays))
		for _, d := range w.ContributionDays {
			parsed, err := time.Parse("2006-01-02", d.Date)
			if err != nil {
				return stats{}, fmt.Errorf("parsing contribution date %q: %w", d.Date, err)
			}
			week = append(week, day{count: d.ContributionCount, date: parsed})
			if d.ContributionCount > s.maxDay {
				s.maxDay = d.ContributionCount
			}
		}
		s.weeks = append(s.weeks, week)
	}

	bytesByLang := map[string]int{}
	for _, repo := range u.Repositories.Nodes {
		s.stars += repo.StargazerCount
		for _, e := range repo.Languages.Edges {
			if excludedLanguages[e.Node.Name] {
				continue
			}
			bytesByLang[e.Node.Name] += e.Size
		}
	}
	s.languages = topLanguages(bytesByLang, 5)

	return s, nil
}

// topLanguages returns the n largest languages by bytes, as a share of the
// total across every language (so the returned percentages sum to <= 100).
func topLanguages(sizes map[string]int, n int) []language {
	type entry struct {
		name string
		size int
	}

	total := 0
	all := make([]entry, 0, len(sizes))
	for name, size := range sizes {
		total += size
		all = append(all, entry{name, size})
	}
	if total == 0 {
		return nil
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].size != all[j].size {
			return all[i].size > all[j].size
		}
		return all[i].name < all[j].name
	})
	if len(all) > n {
		all = all[:n]
	}

	out := make([]language, 0, len(all))
	for _, e := range all {
		out = append(out, language{name: e.name, percent: 100 * float64(e.size) / float64(total)})
	}
	return out
}

// comma formats n with thousands separators: 1284 -> "1,284".
func comma(n int) string {
	if n < 0 {
		return "-" + comma(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	head := len(s) % 3
	if head == 0 {
		head = 3
	}
	out := []string{s[:head]}
	for i := head; i < len(s); i += 3 {
		out = append(out, s[i:i+3])
	}
	return strings.Join(out, ",")
}

// uptime renders the elapsed time since start as "1y 9m 2d".
func uptime(start, now time.Time) string {
	years := now.Year() - start.Year()
	months := int(now.Month()) - int(start.Month())
	days := now.Day() - start.Day()

	if days < 0 {
		months--
		days += daysInPrevMonth(now)
	}
	if months < 0 {
		years--
		months += 12
	}
	return fmt.Sprintf("%dy %dm %dd", years, months, days)
}

func daysInPrevMonth(t time.Time) int {
	firstOfMonth := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	return firstOfMonth.AddDate(0, 0, -1).Day()
}

// setField replaces the text content of the <tspan> carrying the given id.
func setField(svg []byte, id, value string) []byte {
	re := regexp.MustCompile(`(id="` + regexp.QuoteMeta(id) + `"[^>]*>)[^<]*(</tspan>)`)
	return re.ReplaceAll(svg, []byte("${1}"+value+"${2}"))
}

func main() {
	token := os.Getenv("ACCESS_TOKEN")
	if token == "" {
		log.Fatal("ACCESS_TOKEN is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := fetchStats(ctx, token)
	if err != nil {
		log.Fatalf("fetching stats: %v", err)
	}

	start, err := time.Parse("2006-01-02", startDate)
	if err != nil {
		log.Fatalf("parsing start date: %v", err)
	}
	up := uptime(start, time.Now().UTC())

	for _, name := range terminalFiles {
		svg, err := os.ReadFile(name)
		if err != nil {
			log.Fatalf("reading %s: %v", name, err)
		}
		svg = setField(svg, "uptime", up)
		if err := os.WriteFile(name, svg, 0o644); err != nil {
			log.Fatalf("writing %s: %v", name, err)
		}
	}

	cards := map[string]theme{
		"stats_dark.svg":  darkTheme,
		"stats_light.svg": lightTheme,
	}
	for name, t := range cards {
		if err := os.WriteFile(name, []byte(renderStatsCard(s, t)), 0o644); err != nil {
			log.Fatalf("writing %s: %v", name, err)
		}
	}

	log.Printf("%d contributions, %d commits, %d PRs, %d reviews, %d stars, uptime %s",
		s.contributions, s.commits, s.pullRequests, s.reviews, s.stars, up)
}

// ---------------------------------------------------------------- stats card

type theme struct {
	bg     string
	panel  string
	border string
	text   string
	muted  string
	track  string
	heat   [5]string // index 0 is "no contributions"
}

var darkTheme = theme{
	bg:     "#0d1117",
	panel:  "#161b22",
	border: "#30363d",
	text:   "#e6edf3",
	muted:  "#8b949e",
	track:  "#21262d",
	heat:   [5]string{"#161b22", "#0e4429", "#006d32", "#26a641", "#39d353"},
}

var lightTheme = theme{
	bg:     "#ffffff",
	panel:  "#f6f8fa",
	border: "#d0d7de",
	text:   "#1f2328",
	muted:  "#656d76",
	track:  "#eaeef2",
	heat:   [5]string{"#ebedf0", "#9be9a8", "#40c463", "#30a14e", "#216e39"},
}

// langColors mirrors GitHub's linguist palette for the languages likely to
// show up here. Anything unknown falls back to a neutral cycle.
var langColors = map[string]string{
	"Go":         "#00ADD8",
	"Python":     "#3572A5",
	"JavaScript": "#f1e05a",
	"TypeScript": "#3178c6",
	"HTML":       "#e34c26",
	"CSS":        "#563d7c",
	"SCSS":       "#c6538c",
	"Java":       "#b07219",
	"C":          "#555555",
	"C++":        "#f34b7d",
	"C#":         "#178600",
	"Shell":      "#89e051",
	"Dockerfile": "#384d54",
	"Makefile":   "#427819",
	"Rust":       "#dea584",
	"Ruby":       "#701516",
	"PHP":        "#4F5D95",
	"Kotlin":     "#A97BFF",
	"Swift":      "#F05138",
	"Dart":       "#00B4AB",
	"Vue":        "#41b883",
	"Svelte":     "#ff3e00",
	"Lua":        "#000080",
	"Solidity":   "#AA6746",
}

var fallbackColors = []string{"#8957e5", "#db6d28", "#3fb950", "#58a6ff", "#d29922"}

func langColor(name string, i int) string {
	if c, ok := langColors[name]; ok {
		return c
	}
	return fallbackColors[i%len(fallbackColors)]
}

// Card geometry. Width matches the terminal banner so the two stack cleanly.
const (
	cardW   = 660
	padX    = 21.0
	innerW  = cardW - 2*padX
	tileH   = 64.0
	tileGap = 10.0
	cellPit = 11.7 // heatmap column pitch
	cellSz  = 9.7
	legendH = 18.0
)

type tile struct {
	value string
	label string
}

func renderStatsCard(s stats, t theme) string {
	tilesTop := []tile{
		{comma(s.contributions), "CONTRIBUTIONS"},
		{comma(s.commits), "COMMITS"},
		{comma(s.pullRequests), "PULL REQUESTS"},
		{comma(s.reviews), "REVIEWS"},
	}
	tilesBottom := []tile{
		{comma(s.repos), "PUBLIC REPOS"},
		{comma(s.stars), "STARS EARNED"},
		{comma(s.followers), "FOLLOWERS"},
		{comma(s.issues), "ISSUES OPENED"},
	}

	const (
		row1Y   = 52.0
		row2Y   = row1Y + tileH + tileGap
		monthsY = row2Y + tileH + 34 // baseline of the month labels
		gridY   = monthsY + 8
	)
	gridH := 7*cellPit - (cellPit - cellSz)
	langLabelY := gridY + gridH + 30
	barY := langLabelY + 10

	legendLines := legendRowCount(s.languages)
	cardH := int(math.Ceil(barY + 12 + 20 + float64(legendLines)*legendH + 8))

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" `+
		`font-family="ui-monospace,SFMono-Regular,&quot;Cascadia Code&quot;,Menlo,Consolas,monospace">`+"\n",
		cardW, cardH, cardW, cardH)
	fmt.Fprintf(&b, `  <rect x="0.5" y="0.5" width="%d" height="%d" rx="10" fill="%s" stroke="%s"/>`+"\n",
		cardW-1, cardH-1, t.bg, t.border)

	// Header.
	fmt.Fprintf(&b, `  <text x="%.0f" y="32" fill="%s" font-size="14" font-weight="600">GitHub activity</text>`+"\n",
		padX, t.text)
	fmt.Fprintf(&b, `  <text x="%.0f" y="32" fill="%s" font-size="11" text-anchor="end">last 12 months</text>`+"\n",
		padX+innerW, t.muted)

	writeTileRow(&b, t, tilesTop, row1Y)
	writeTileRow(&b, t, tilesBottom, row2Y)

	writeHeatmap(&b, t, s, monthsY, gridY)

	// Language bar.
	fmt.Fprintf(&b, `  <text x="%.0f" y="%.0f" fill="%s" font-size="11">Most used languages</text>`+"\n",
		padX, langLabelY, t.muted)
	writeLanguageBar(&b, t, s.languages, barY, barY+12+20)

	b.WriteString("</svg>\n")
	return b.String()
}

func writeTileRow(b *strings.Builder, t theme, tiles []tile, y float64) {
	w := (innerW - float64(len(tiles)-1)*tileGap) / float64(len(tiles))
	for i, tl := range tiles {
		x := padX + float64(i)*(w+tileGap)
		fmt.Fprintf(b, `  <rect x="%.1f" y="%.0f" width="%.1f" height="%.0f" rx="8" fill="%s" stroke="%s"/>`+"\n",
			x, y, w, tileH, t.panel, t.border)
		fmt.Fprintf(b, `  <text x="%.1f" y="%.0f" fill="%s" font-size="22" font-weight="600">%s</text>`+"\n",
			x+14, y+31, t.text, tl.value)
		fmt.Fprintf(b, `  <text x="%.1f" y="%.0f" fill="%s" font-size="9" letter-spacing="0.6">%s</text>`+"\n",
			x+14, y+49, t.muted, tl.label)
	}
}

// writeHeatmap draws the contribution calendar: one column per week, one cell
// per day, shaded in five buckets relative to the busiest day of the year.
func writeHeatmap(b *strings.Builder, t theme, s stats, monthsY, gridY float64) {
	lastMonth := time.Month(0)
	for i, week := range s.weeks {
		if len(week) == 0 {
			continue
		}
		x := padX + float64(i)*cellPit
		// Label a column when it is the first one to land in a new month.
		if d := week[0].date; d.Month() != lastMonth && d.Day() <= 7 {
			lastMonth = d.Month()
			if i < len(s.weeks)-2 {
				fmt.Fprintf(b, `  <text x="%.1f" y="%.0f" fill="%s" font-size="9">%s</text>`+"\n",
					x, monthsY, t.muted, d.Format("Jan"))
			}
		}
		for _, dy := range week {
			y := gridY + float64(int(dy.date.Weekday()))*cellPit
			fmt.Fprintf(b, `  <rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" fill="%s"/>`+"\n",
				x, y, cellSz, cellSz, t.heat[heatLevel(dy.count, s.maxDay)])
		}
	}
}

// heatLevel buckets a day's count into 0..4 relative to the busiest day.
func heatLevel(count, max int) int {
	if count == 0 || max == 0 {
		return 0
	}
	switch ratio := float64(count) / float64(max); {
	case ratio <= 0.25:
		return 1
	case ratio <= 0.50:
		return 2
	case ratio <= 0.75:
		return 3
	default:
		return 4
	}
}

func writeLanguageBar(b *strings.Builder, t theme, langs []language, barY, legendY float64) {
	fmt.Fprintf(b, `  <clipPath id="barclip"><rect x="%.0f" y="%.0f" width="%.0f" height="12" rx="6"/></clipPath>`+"\n",
		padX, barY, innerW)
	fmt.Fprintf(b, `  <rect x="%.0f" y="%.0f" width="%.0f" height="12" rx="6" fill="%s"/>`+"\n",
		padX, barY, innerW, t.track)

	// Segments are scaled to fill the bar, so the widths compare the top
	// languages against each other rather than against the long tail.
	total := 0.0
	for _, l := range langs {
		total += l.percent
	}
	if total > 0 {
		x := padX
		for i, l := range langs {
			w := innerW * l.percent / total
			fmt.Fprintf(b, `  <rect x="%.2f" y="%.0f" width="%.2f" height="12" fill="%s" clip-path="url(#barclip)"/>`+"\n",
				x, barY, w, langColor(l.name, i))
			x += w
		}
	}

	x, y := padX, legendY
	for i, l := range langs {
		label := fmt.Sprintf("%s %.1f%%", l.name, l.percent)
		w := legendWidth(label)
		if x+w > padX+innerW {
			x, y = padX, y+legendH
		}
		fmt.Fprintf(b, `  <circle cx="%.1f" cy="%.1f" r="4" fill="%s"/>`+"\n",
			x+4, y-4, langColor(l.name, i))
		fmt.Fprintf(b, `  <text x="%.1f" y="%.1f" fill="%s" font-size="11">%s</text>`+"\n",
			x+14, y, t.muted, html.EscapeString(label))
		x += w
	}
}

// legendWidth approximates the rendered width of a legend entry: dot, gap,
// monospace label at 11px (advance is 0.6em), then trailing space.
func legendWidth(label string) float64 {
	return 14 + float64(len([]rune(label)))*6.6 + 18
}

func legendRowCount(langs []language) int {
	if len(langs) == 0 {
		return 1
	}
	rows, x := 1, padX
	for _, l := range langs {
		w := legendWidth(fmt.Sprintf("%s %.1f%%", l.name, l.percent))
		if x+w > padX+innerW {
			rows++
			x = padX
		}
		x += w
	}
	return rows
}
