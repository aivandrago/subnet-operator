/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sheets

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/option"
	sheetsapi "google.golang.org/api/sheets/v4"
)

// fakeSheets is a minimal stand-in for the Sheets API that records what the service sends.
type fakeSheets struct {
	existingTitle string
	rowCount      int64
	protected     bool

	values      *sheetsapi.ValueRange
	valuesRange string
	cleared     string
	batches     []*sheetsapi.BatchUpdateSpreadsheetRequest
}

func (f *fakeSheets) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/v4/spreadsheets/sheet-1"):
			resp := &sheetsapi.Spreadsheet{}
			if f.existingTitle != "" {
				sheet := &sheetsapi.Sheet{Properties: &sheetsapi.SheetProperties{
					SheetId: 7, Title: f.existingTitle,
					GridProperties: &sheetsapi.GridProperties{RowCount: f.rowCount},
				}}
				if f.protected {
					sheet.ProtectedRanges = []*sheetsapi.ProtectedRange{{Description: protectionDescription}}
				}
				resp.Sheets = []*sheetsapi.Sheet{sheet}
			}
			_ = json.NewEncoder(w).Encode(resp)
		case strings.HasSuffix(path, ":batchUpdate"):
			var req sheetsapi.BatchUpdateSpreadsheetRequest
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("bad batchUpdate body: %v", err)
			}
			f.batches = append(f.batches, &req)
			resp := &sheetsapi.BatchUpdateSpreadsheetResponse{}
			for _, rq := range req.Requests {
				if rq.AddSheet != nil {
					resp.Replies = append(resp.Replies, &sheetsapi.Response{
						AddSheet: &sheetsapi.AddSheetResponse{Properties: &sheetsapi.SheetProperties{SheetId: 9, Title: rq.AddSheet.Properties.Title}},
					})
				}
			}
			_ = json.NewEncoder(w).Encode(resp)
		case strings.HasSuffix(path, ":clear"):
			f.cleared = strings.TrimSuffix(path[strings.LastIndex(path, "/values/")+len("/values/"):], ":clear")
			_ = json.NewEncoder(w).Encode(&sheetsapi.ClearValuesResponse{})
		case r.Method == http.MethodPut:
			var vr sheetsapi.ValueRange
			if err := json.Unmarshal(body, &vr); err != nil {
				t.Errorf("bad values body: %v", err)
			}
			f.values = &vr
			f.valuesRange = path[strings.LastIndex(path, "/values/")+len("/values/"):]
			_ = json.NewEncoder(w).Encode(&sheetsapi.UpdateValuesResponse{})
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newTestService(t *testing.T, fake *fakeSheets) *Service {
	t.Helper()
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	s, err := newService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func table() Table {
	return Table{Header: []string{"Account", "Subnet"}, Rows: [][]string{{"111111111111", "subnet-1"}}}
}

func TestSyncExistingSheet(t *testing.T) {
	// The sheet held 10 rows before; now it holds a header and one row.
	fake := &fakeSheets{existingTitle: "Subnets", rowCount: 10}
	if err := newTestService(t, fake).Sync(context.Background(), "sheet-1", "Subnets", table()); err != nil {
		t.Fatal(err)
	}

	if want := "'Subnets'!A1"; fake.valuesRange != want {
		t.Errorf("values written to %q, want %q", fake.valuesRange, want)
	}
	if fake.values == nil || len(fake.values.Values) != 2 || fake.values.Values[0][0] != "Account" {
		t.Fatalf("unexpected values %+v", fake.values)
	}
	if want := "'Subnets'!A3:ZZ10"; fake.cleared != want {
		t.Errorf("cleared %q, want %q (rows left from the previous export)", fake.cleared, want)
	}
	if len(fake.batches) != 1 {
		t.Fatalf("want one formatting batch, got %d", len(fake.batches))
	}
	var frozen, bold, protect bool
	for _, rq := range fake.batches[0].Requests {
		frozen = frozen || (rq.UpdateSheetProperties != nil && rq.UpdateSheetProperties.Properties.GridProperties.FrozenRowCount == 1)
		bold = bold || (rq.RepeatCell != nil && rq.RepeatCell.Cell.UserEnteredFormat.TextFormat.Bold)
		protect = protect || rq.AddProtectedRange != nil
	}
	if !frozen || !bold || !protect {
		t.Errorf("formatting batch missing something: frozen=%v bold=%v protect=%v", frozen, bold, protect)
	}
}

func TestSyncCreatesSheetAndKeepsProtection(t *testing.T) {
	fake := &fakeSheets{}
	if err := newTestService(t, fake).Sync(context.Background(), "sheet-1", "My sheet", table()); err != nil {
		t.Fatal(err)
	}
	if len(fake.batches) != 2 || fake.batches[0].Requests[0].AddSheet == nil {
		t.Fatalf("the missing tab must be created first, got %d batches", len(fake.batches))
	}
	if fake.cleared != "" {
		t.Errorf("a new tab has nothing to clear, cleared %q", fake.cleared)
	}
	if want := "'My sheet'!A1"; fake.valuesRange != want {
		t.Errorf("values written to %q, want %q", fake.valuesRange, want)
	}

	// A sheet that is already protected must not collect a second protected range.
	fake2 := &fakeSheets{existingTitle: "Subnets", rowCount: 2, protected: true}
	if err := newTestService(t, fake2).Sync(context.Background(), "sheet-1", "Subnets", table()); err != nil {
		t.Fatal(err)
	}
	for _, rq := range fake2.batches[0].Requests {
		if rq.AddProtectedRange != nil {
			t.Error("protection added twice")
		}
	}
}

func TestNewRejectsNonServiceAccountCredentials(t *testing.T) {
	cases := map[string]string{
		"external account": `{"type":"external_account","credential_source":{"executable":{"command":"/bin/sh"}}}`,
		"authorized user":  `{"type":"authorized_user"}`,
		"garbage":          `not json`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(context.Background(), []byte(body)); err == nil {
				t.Error("want an error, got none")
			}
		})
	}
}
