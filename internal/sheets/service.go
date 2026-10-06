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
	"fmt"
	"strings"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	sheetsapi "google.golang.org/api/sheets/v4"
)

// protectionDescription marks the range the operator protects, so people see a warning
// before they edit a sheet that is overwritten on every export.
const protectionDescription = "Maintained by subnet-operator - edits are overwritten"

// legacyProtectionDescription is what releases before the rename wrote. A sheet protected by
// one of them is protected already, and must not get a second range.
const legacyProtectionDescription = "Maintained by aws-subnet-operator - edits are overwritten"

// Service writes tables with the Google Sheets API.
type Service struct {
	api *sheetsapi.Service
}

var _ Syncer = (*Service)(nil)

// New builds a service from a Google service account key.
//
// Only service account keys are accepted. Other credential configurations, such as external
// account files, can make the client call out to a local command or URL, which must not be
// possible through a Secret the operator merely reads.
func New(ctx context.Context, credentialsJSON []byte) (*Service, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(credentialsJSON, &probe); err != nil {
		return nil, fmt.Errorf("credentials are not valid JSON: %w", err)
	}
	if probe.Type != "service_account" {
		return nil, fmt.Errorf("credentials of type %q are not supported, use a service account key", probe.Type)
	}
	cfg, err := google.JWTConfigFromJSON(credentialsJSON, sheetsapi.SpreadsheetsScope)
	if err != nil {
		return nil, fmt.Errorf("read Google service account key: %w", err)
	}
	return newService(ctx, option.WithTokenSource(cfg.TokenSource(ctx)))
}

func newService(ctx context.Context, opts ...option.ClientOption) (*Service, error) {
	api, err := sheetsapi.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create sheets client: %w", err)
	}
	return &Service{api: api}, nil
}

// Sync rewrites the tab with the table: it creates the tab if needed, writes header and rows,
// clears whatever was below them, freezes and bolds the header and protects the range.
func (s *Service) Sync(ctx context.Context, spreadsheetID, sheetName string, t Table) error {
	sheet, err := s.ensureSheet(ctx, spreadsheetID, sheetName)
	if err != nil {
		return err
	}

	values := make([][]any, 0, len(t.Rows)+1)
	values = append(values, toAny(t.Header))
	for _, r := range t.Rows {
		values = append(values, toAny(r))
	}
	written := int64(len(values))

	if _, err := s.api.Spreadsheets.Values.Update(spreadsheetID, a1(sheetName, "A1"),
		&sheetsapi.ValueRange{Values: values}).
		ValueInputOption("RAW").Context(ctx).Do(); err != nil {
		return fmt.Errorf("write values: %w", err)
	}

	// Rows left over from a previous, longer export would look like current data.
	if previous := sheet.Properties.GridProperties.RowCount; previous > written {
		clearRange := a1(sheetName, fmt.Sprintf("A%d:ZZ%d", written+1, previous))
		if _, err := s.api.Spreadsheets.Values.Clear(spreadsheetID, clearRange,
			&sheetsapi.ClearValuesRequest{}).Context(ctx).Do(); err != nil {
			return fmt.Errorf("clear stale rows: %w", err)
		}
	}

	return s.format(ctx, spreadsheetID, sheet, int64(len(t.Header)))
}

// ensureSheet returns the tab, creating it when it does not exist yet.
func (s *Service) ensureSheet(ctx context.Context, spreadsheetID, sheetName string) (*sheetsapi.Sheet, error) {
	ss, err := s.api.Spreadsheets.Get(spreadsheetID).
		Fields("sheets(properties,protectedRanges)").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("read spreadsheet: %w", err)
	}
	for _, sheet := range ss.Sheets {
		if sheet.Properties != nil && sheet.Properties.Title == sheetName {
			if sheet.Properties.GridProperties == nil {
				sheet.Properties.GridProperties = &sheetsapi.GridProperties{}
			}
			return sheet, nil
		}
	}

	resp, err := s.api.Spreadsheets.BatchUpdate(spreadsheetID, &sheetsapi.BatchUpdateSpreadsheetRequest{
		Requests: []*sheetsapi.Request{{
			AddSheet: &sheetsapi.AddSheetRequest{Properties: &sheetsapi.SheetProperties{Title: sheetName}},
		}},
	}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("create sheet %q: %w", sheetName, err)
	}
	for _, r := range resp.Replies {
		if r.AddSheet != nil && r.AddSheet.Properties != nil {
			props := r.AddSheet.Properties
			if props.GridProperties == nil {
				props.GridProperties = &sheetsapi.GridProperties{}
			}
			return &sheetsapi.Sheet{Properties: props}, nil
		}
	}
	return nil, fmt.Errorf("creating sheet %q returned no properties", sheetName)
}

// format freezes and bolds the header row and, once, protects the tab with a warning.
func (s *Service) format(ctx context.Context, spreadsheetID string, sheet *sheetsapi.Sheet, columns int64) error {
	id := sheet.Properties.SheetId
	requests := []*sheetsapi.Request{
		{UpdateSheetProperties: &sheetsapi.UpdateSheetPropertiesRequest{
			Properties: &sheetsapi.SheetProperties{
				SheetId:        id,
				GridProperties: &sheetsapi.GridProperties{FrozenRowCount: 1},
			},
			Fields: "gridProperties.frozenRowCount",
		}},
		{RepeatCell: &sheetsapi.RepeatCellRequest{
			Range: &sheetsapi.GridRange{SheetId: id, StartRowIndex: 0, EndRowIndex: 1, StartColumnIndex: 0, EndColumnIndex: columns},
			Cell: &sheetsapi.CellData{UserEnteredFormat: &sheetsapi.CellFormat{
				TextFormat: &sheetsapi.TextFormat{Bold: true},
			}},
			Fields: "userEnteredFormat.textFormat.bold",
		}},
	}
	if !hasProtection(sheet) {
		requests = append(requests, &sheetsapi.Request{
			AddProtectedRange: &sheetsapi.AddProtectedRangeRequest{
				ProtectedRange: &sheetsapi.ProtectedRange{
					Range:       &sheetsapi.GridRange{SheetId: id},
					Description: protectionDescription,
					WarningOnly: true,
				},
			},
		})
	}
	if _, err := s.api.Spreadsheets.BatchUpdate(spreadsheetID,
		&sheetsapi.BatchUpdateSpreadsheetRequest{Requests: requests}).Context(ctx).Do(); err != nil {
		return fmt.Errorf("format sheet: %w", err)
	}
	return nil
}

func hasProtection(sheet *sheetsapi.Sheet) bool {
	for _, p := range sheet.ProtectedRanges {
		if p.Description == protectionDescription || p.Description == legacyProtectionDescription {
			return true
		}
	}
	return false
}

// a1 quotes the tab name for an A1 range: 'My sheet'!A1.
func a1(sheetName, ref string) string {
	return "'" + strings.ReplaceAll(sheetName, "'", "''") + "'!" + ref
}

func toAny(row []string) []any {
	out := make([]any, len(row))
	for i, v := range row {
		out[i] = v
	}
	return out
}

// URL is the link to a spreadsheet tab, for the export status.
func URL(spreadsheetID string) string {
	return "https://docs.google.com/spreadsheets/d/" + spreadsheetID + "/edit"
}
