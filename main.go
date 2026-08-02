// Command profilegen refreshes the dynamic fields in the profile SVGs.
//
// It queries the GitHub GraphQL API for account-wide contribution stats,
// computes service uptime since the start date, and rewrites the <tspan>
// placeholders in dark_mode.svg and light_mode.svg in place.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
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

var files = []string{"dark_mode.svg", "light_mode.svg"}

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
      contributionCalendar{ totalContributions }
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

// stats holds every value rendered into the terminal's `gh stats` block.
type stats struct {
	contributions int
	commits       int
	pullRequests  int
	reviews       int
	issues        int
	repos         int
	stars         int
	followers     int
	languages     string
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

	bytesByLang := map[string]int{}
	for _, repo := range u.Repositories.Nodes {
		s.stars += repo.StargazerCount
		for _, e := range repo.Languages.Edges {
			bytesByLang[e.Node.Name] += e.Size
		}
	}
	s.languages = topLanguages(bytesByLang, 3)

	return s, nil
}

// topLanguages renders the n largest languages by bytes as "Go 46% · Python 31%".
// Percentages are of the total across every language, so they need not sum to 100.
func topLanguages(sizes map[string]int, n int) string {
	type lang struct {
		name string
		size int
	}

	total := 0
	all := make([]lang, 0, len(sizes))
	for name, size := range sizes {
		total += size
		all = append(all, lang{name, size})
	}
	if total == 0 {
		return "—"
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

	parts := make([]string, 0, len(all))
	for _, l := range all {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", l.name, 100*float64(l.size)/float64(total)))
	}
	return strings.Join(parts, " · ")
}

// comma formats n with thousands separators: 1284 -> "1,284".
func comma(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return "-" + comma(-n)
	}
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

	fields := map[string]string{
		"uptime":       up,
		"contrib_year": comma(s.contributions),
		"commits_year": comma(s.commits),
		"prs_year":     comma(s.pullRequests),
		"reviews_year": comma(s.reviews),
		"issues_year":  comma(s.issues),
		"repos":        comma(s.repos),
		"stars":        comma(s.stars),
		"followers":    comma(s.followers),
		"langs":        s.languages,
	}

	for _, name := range files {
		svg, err := os.ReadFile(name)
		if err != nil {
			log.Fatalf("reading %s: %v", name, err)
		}

		for id, value := range fields {
			svg = setField(svg, id, value)
		}

		if err := os.WriteFile(name, svg, 0o644); err != nil {
			log.Fatalf("writing %s: %v", name, err)
		}
	}

	log.Printf("updated %d files: %d contributions, %d commits, %d PRs, %d stars, uptime %s",
		len(files), s.contributions, s.commits, s.pullRequests, s.stars, up)
}
