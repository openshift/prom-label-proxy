// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package injectproxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/prometheus/prometheus/model/labels"
)

type apiResponse struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data,omitempty"`
	ErrorType string          `json:"errorType,omitempty"`
	Error     string          `json:"error,omitempty"`
	Warnings  []string        `json:"warnings,omitempty"`
}

func getAPIResponse(resp *http.Response) (*apiResponse, error) {
	defer resp.Body.Close()
	reader := resp.Body

	if resp.Header.Get("Content-Encoding") == "gzip" && !resp.Uncompressed {
		var err error
		reader, err = gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip decoding error: %w", err)
		}
		defer reader.Close()

		// TODO: recompress the modified response?
		resp.Header.Del("Content-Encoding")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var apir apiResponse
	if err := json.NewDecoder(reader).Decode(&apir); err != nil {
		return nil, fmt.Errorf("JSON decoding error: %w", err)
	}

	if apir.Status != "success" {
		return nil, fmt.Errorf("unexpected response status: %q", apir.Status)
	}

	return &apir, nil
}

type rulesData struct {
	RuleGroups []*ruleGroup `json:"groups"`
}

type ruleGroup struct {
	Name     string  `json:"name"`
	File     string  `json:"file"`
	Rules    []rule  `json:"rules"`
	Interval float64 `json:"interval"`
}

type rule struct {
	*alertingRule
	*recordingRule
}

func (r *rule) Labels() labels.Labels {
	if r.alertingRule != nil {
		return r.alertingRule.Labels
	}
	return r.recordingRule.Labels
}

// MarshalJSON implements the json.Marshaler interface for rule.
func (r *rule) MarshalJSON() ([]byte, error) {
	if r.alertingRule != nil {
		return json.Marshal(r.alertingRule)
	}
	return json.Marshal(r.recordingRule)
}

// UnmarshalJSON implements the json.Unmarshaler interface for rule.
func (r *rule) UnmarshalJSON(b []byte) error {
	var ruleType struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &ruleType); err != nil {
		return err
	}
	switch ruleType.Type {
	case "alerting":
		var alertingr alertingRule
		if err := json.Unmarshal(b, &alertingr); err != nil {
			return err
		}
		r.alertingRule = &alertingr
	case "recording":
		var recordingr recordingRule
		if err := json.Unmarshal(b, &recordingr); err != nil {
			return err
		}
		r.recordingRule = &recordingr
	default:
		return fmt.Errorf("failed to unmarshal rule: unknown type %q", ruleType.Type)
	}

	return nil
}

type alertingRule struct {
	State          string        `json:"state"`
	Name           string        `json:"name"`
	Query          string        `json:"query"`
	Duration       float64       `json:"duration"`
	KeepFiringFor  float64       `json:"keepFiringFor"`
	Labels         labels.Labels `json:"labels"`
	Annotations    labels.Labels `json:"annotations"`
	Alerts         []*alert      `json:"alerts"`
	Health         string        `json:"health"`
	LastError      string        `json:"lastError,omitempty"`
	EvaluationTime float64       `json:"evaluationTime"`
	LastEvaluation time.Time     `json:"lastEvaluation"`
	// Type of an alertingRule is always "alerting".
	Type string `json:"type"`
}

type recordingRule struct {
	Name           string        `json:"name"`
	Query          string        `json:"query"`
	Labels         labels.Labels `json:"labels,omitempty"`
	Health         string        `json:"health"`
	LastError      string        `json:"lastError,omitempty"`
	EvaluationTime float64       `json:"evaluationTime"`
	LastEvaluation time.Time     `json:"lastEvaluation"`
	// Type of a recordingRule is always "recording".
	Type string `json:"type"`
}

type alertsData struct {
	Alerts []*alert `json:"alerts"`
}

type alert struct {
	Labels          labels.Labels `json:"labels"`
	Annotations     labels.Labels `json:"annotations"`
	State           string        `json:"state"`
	ActiveAt        *time.Time    `json:"activeAt,omitempty"`
	KeepFiringSince *time.Time    `json:"keepFiringSince,omitempty"`
	Value           string        `json:"value"`
}

// errModifyResponseFailed is returned when the proxy failed to modify the
// response from the backend.
var errModifyResponseFailed = errors.New("failed to process the API response")

// modifyAPIResponse unwraps the Prometheus API response, passes the enforced
// label value and the response to the given function and finally replaces the
// result in the response.
func modifyAPIResponse(f func([]string, *http.Request, *apiResponse) (interface{}, error)) func(*http.Response) error {
	return func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK {
			// Pass non-200 responses as-is.
			return nil
		}

		apir, err := getAPIResponse(resp)
		if err != nil {
			return fmt.Errorf("can't decode the response: %w", err)
		}

		v, err := f(MustLabelValues(resp.Request.Context()), resp.Request, apir)
		if err != nil {
			return fmt.Errorf("%w: %w", errModifyResponseFailed, err)
		}

		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("can't encode the data: %w", err)
		}

		apir.Data = json.RawMessage(b)

		var buf bytes.Buffer
		if err = json.NewEncoder(&buf).Encode(apir); err != nil {
			return fmt.Errorf("can't encode the response: %w", err)
		}
		resp.Body = io.NopCloser(&buf)
		resp.Header["Content-Length"] = []string{fmt.Sprint(buf.Len())}

		return nil
	}
}

func (r *routes) filterRules(lvalues []string, req *http.Request, resp *apiResponse) (interface{}, error) {
	var rgs rulesData
	if err := json.Unmarshal(resp.Data, &rgs); err != nil {
		return nil, fmt.Errorf("can't decode rules data: %w", err)
	}

	m, err := r.newLabelMatcher(lvalues...)
	if err != nil {
		return nil, err
	}

	filtered := []*ruleGroup{}
	for _, rg := range rgs.RuleGroups {
		var rules []rule
		for _, rgr := range rg.Rules {
			if lval := rgr.Labels().Get(r.label); lval != "" && m.Matches(lval) {
				rules = append(rules, rgr)
				continue
			}

			if !r.rulesWithActiveAlerts || rgr.alertingRule == nil {
				continue
			}

			var ar *alertingRule
			for i := range rgr.Alerts {
				if lval := rgr.Alerts[i].Labels.Get(r.label); lval == "" || !m.Matches(lval) {
					continue
				}

				if ar == nil {
					ar = &alertingRule{
						Name:           rgr.alertingRule.Name,
						Query:          rgr.alertingRule.Query,
						Duration:       rgr.Duration,
						KeepFiringFor:  rgr.KeepFiringFor,
						Labels:         rgr.alertingRule.Labels.Copy(),
						Annotations:    rgr.Annotations.Copy(),
						Health:         rgr.alertingRule.Health,
						LastError:      rgr.alertingRule.LastError,
						EvaluationTime: rgr.alertingRule.EvaluationTime,
						LastEvaluation: rgr.alertingRule.LastEvaluation,
						Type:           rgr.alertingRule.Type,
					}
				}

				ar.Alerts = append(ar.Alerts, rgr.Alerts[i])
				switch ar.State {
				case "pending":
					if rgr.alertingRule.Alerts[i].State == "firing" {
						ar.State = rgr.alertingRule.Alerts[i].State
					}
				case "":
					ar.State = rgr.alertingRule.Alerts[i].State
				}
			}

			if ar != nil {
				rules = append(rules, rule{alertingRule: ar})
			}
		}

		if len(rules) > 0 {
			rg.Rules = rules
			filtered = append(filtered, rg)
		}
	}

	return &rulesData{RuleGroups: filtered}, nil
}

func (r *routes) filterAlerts(lvalues []string, _ *http.Request, resp *apiResponse) (interface{}, error) {
	var data alertsData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, fmt.Errorf("can't decode alerts data: %w", err)
	}

	m, err := r.newLabelMatcher(lvalues...)
	if err != nil {
		return nil, err
	}

	filtered := []*alert{}
	for _, alert := range data.Alerts {
		if lval := alert.Labels.Get(r.label); lval != "" && m.Matches(lval) {
			filtered = append(filtered, alert)
		}
	}

	return &alertsData{Alerts: filtered}, nil
}
