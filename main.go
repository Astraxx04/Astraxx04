// Command profilegen refreshes the dynamic fields in the profile SVGs.
//
// It queries the GitHub GraphQL API for the commit count on prrev, computes
// service uptime since the start date, and rewrites the <tspan> placeholders
// in dark_mode.svg and light_mode.svg in place.
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
	"time"
)

const (
	startDate = "2024-11-01"
	repoOwner = "Astraxx04"
	repoName  = "prrev"
	endpoint  = "https://api.github.com/graphql"
)

var files = []string{"dark_mode.svg", "light_mode.svg"}

const query = `query($owner:String!,$name:String!){
  repository(owner:$owner,name:$name){
    defaultBranchRef{ target{ ... on Commit { history { totalCount } } } }
  }
}`

type gqlResp struct {
	Data struct {
		Repository struct {
			DefaultBranchRef struct {
				Target struct {
					History struct {
						TotalCount int `json:"totalCount"`
					} `json:"history"`
				} `json:"target"`
			} `json:"defaultBranchRef"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func commitCount(ctx context.Context, token string) (int, error) {
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]string{"owner": repoOwner, "name": repoName},
	})
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("github graphql: unexpected status %s", resp.Status)
	}

	var out gqlResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	if len(out.Errors) > 0 {
		return 0, fmt.Errorf("github graphql: %s", out.Errors[0].Message)
	}
	return out.Data.Repository.DefaultBranchRef.Target.History.TotalCount, nil
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

	commits, err := commitCount(ctx, token)
	if err != nil {
		log.Fatalf("fetching commit count: %v", err)
	}

	start, err := time.Parse("2006-01-02", startDate)
	if err != nil {
		log.Fatalf("parsing start date: %v", err)
	}
	up := uptime(start, time.Now().UTC())

	for _, name := range files {
		svg, err := os.ReadFile(name)
		if err != nil {
			log.Fatalf("reading %s: %v", name, err)
		}

		svg = setField(svg, "prrev_commits", fmt.Sprintf("%d", commits))
		svg = setField(svg, "uptime", up)

		if err := os.WriteFile(name, svg, 0o644); err != nil {
			log.Fatalf("writing %s: %v", name, err)
		}
	}

	log.Printf("updated %d files: %d commits, uptime %s", len(files), commits, up)
}
