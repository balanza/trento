package gh

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseWorkflowRunsList(t *testing.T) {
	raw := `[
      {"id":111,"name":"CI","status":"completed","conclusion":"success","head_sha":"abc","html_url":"https://x/runs/111"},
      {"id":222,"name":"Build","status":"in_progress","conclusion":"","head_sha":"abc","html_url":"https://x/runs/222"}
    ]`
	got, err := parseWorkflowRunsJSON([]byte(raw))
	assert.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Equal(t, int64(111), got[0].ID)
	assert.Equal(t, "CI", got[0].Name)
	assert.Equal(t, "completed", got[0].Status)
	assert.Equal(t, "success", got[0].Conclusion)
	assert.Equal(t, "abc", got[0].HeadSHA)
	assert.Equal(t, "https://x/runs/111", got[0].URL)
	assert.Equal(t, "", got[1].Conclusion)
}

func TestParseJobsForRun(t *testing.T) {
	raw := `{"jobs":[
      {"id":900,"run_id":111,"name":"test","status":"completed","conclusion":"failure",
       "steps":[
         {"name":"Set up","number":1,"conclusion":"success"},
         {"name":"Run tests","number":2,"conclusion":"failure"}
       ]}
    ]}`
	got, err := parseJobsForRunJSON([]byte(raw))
	assert.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Equal(t, int64(900), got[0].ID)
	assert.Equal(t, int64(111), got[0].RunID)
	assert.Equal(t, "failure", got[0].Conclusion)
	assert.Len(t, got[0].Steps, 2)
	assert.Equal(t, "failure", got[0].Steps[1].Conclusion)
}

func TestParseMilestonesJSON(t *testing.T) {
	raw := `[
      {"number":7,"title":"2.4.7","open_issues":3,"closed_issues":0},
      {"number":8,"title":"2.4.8","open_issues":0,"closed_issues":0}
    ]`
	got, err := parseMilestonesJSON([]byte(raw))
	assert.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Equal(t, int64(7), got[0].Number)
	assert.Equal(t, "2.4.7", got[0].Title)
	assert.Equal(t, int64(8), got[1].Number)
	assert.Equal(t, "2.4.8", got[1].Title)
}
