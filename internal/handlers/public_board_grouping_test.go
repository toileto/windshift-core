package handlers

import (
	"testing"

	"windshift/internal/models"
)

func intPtr(value int) *int { return &value }

func TestResolvePublicEffectiveProjects(t *testing.T) {
	items := []models.Item{
		{ID: 1, ProjectID: intPtr(7), ProjectName: "Apollo", InheritProject: false},
		{ID: 2, ParentID: intPtr(1), InheritProject: true},
		{ID: 3, ParentID: intPtr(1), InheritProject: false},
		{ID: 4, ParentID: intPtr(99), InheritProject: true}, // parent not loaded
		{ID: 5, ParentID: intPtr(5), InheritProject: true},  // self-cycle guard
	}

	resolved := resolvePublicEffectiveProjects(items)

	if got := resolved[1]; got == nil || got.id != 7 || got.name != "Apollo" {
		t.Fatalf("direct project = %+v, want {id:7 name:Apollo}", got)
	}
	if got := resolved[2]; got == nil || got.id != 7 {
		t.Fatalf("inherited project = %+v, want id 7 from parent", got)
	}
	if got := resolved[3]; got != nil {
		t.Fatalf("explicit none = %+v, want nil", got)
	}
	if got := resolved[4]; got != nil {
		t.Fatalf("unloadable parent = %+v, want nil", got)
	}
	if got := resolved[5]; got != nil {
		t.Fatalf("self cycle = %+v, want nil", got)
	}
}

func TestPublicLaneKeyForCard(t *testing.T) {
	statusID := 12
	assigneeID := 3

	tests := []struct {
		name      string
		groupBy   string
		item      *models.Item
		effective *publicEffectiveProject
		want      string
	}{
		{
			name:    "flat boards carry no lane key",
			groupBy: "",
			item:    &models.Item{},
			want:    "",
		},
		{
			name:    "assignee lane",
			groupBy: "assignee",
			item:    &models.Item{AssigneeID: &assigneeID},
			want:    "assignee-3",
		},
		{
			name:    "unassigned assignee",
			groupBy: "assignee",
			item:    &models.Item{},
			want:    "none",
		},
		{
			name:    "status lane",
			groupBy: "status",
			item:    &models.Item{StatusID: &statusID},
			want:    "status-12",
		},
		{
			name:      "project lane uses the resolved project",
			groupBy:   "project",
			item:      &models.Item{},
			effective: &publicEffectiveProject{id: 7, name: "Apollo"},
			want:      "project-7",
		},
		{
			name:    "no project",
			groupBy: "project",
			item:    &models.Item{},
			want:    "none",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := publicLaneKeyForCard(test.groupBy, test.item, test.effective); got != test.want {
				t.Errorf("publicLaneKeyForCard() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBuildPublicBoardLanesAssignee(t *testing.T) {
	columns := []boardColumnInfo{{name: "Open"}, {name: "Closed"}}
	responseColumns := []publicColumn{
		{Items: []publicBoardCard{
			{AssigneeName: "Bo Aldrin", LaneKey: "assignee-2"},
			{LaneKey: "none"},
		}},
		{Items: []publicBoardCard{
			{AssigneeName: "Ada Lovelace", LaneKey: "assignee-1"},
			{AssigneeName: "Bo Aldrin", LaneKey: "assignee-2"},
		}},
	}

	lanes := buildPublicBoardLanes("assignee", columns, responseColumns)
	if len(lanes) != 3 {
		t.Fatalf("lanes = %+v, want two assignees plus unassigned", lanes)
	}
	if lanes[0].Key != "assignee-1" || lanes[0].Title != "Ada Lovelace" {
		t.Errorf("first lane = %+v, want assignee-1 Ada Lovelace", lanes[0])
	}
	if lanes[1].Key != "assignee-2" || lanes[1].Title != "Bo Aldrin" {
		t.Errorf("second lane = %+v, want assignee-2 Bo Aldrin", lanes[1])
	}
	if lanes[2].Key != "none" || lanes[2].Title != "Unassigned" {
		t.Errorf("last lane = %+v, want the unassigned lane", lanes[2])
	}
}

func TestBuildPublicBoardLanesStatusFollowsColumnOrder(t *testing.T) {
	columns := []boardColumnInfo{
		{name: "Open", statusIDs: []int{10, 11}},
		{name: "Closed", statusIDs: []int{12}},
	}
	responseColumns := []publicColumn{
		{Items: []publicBoardCard{
			{StatusName: "In progress", LaneKey: "status-11"},
			{StatusName: "To do", LaneKey: "status-10"},
		}},
		{Items: []publicBoardCard{
			{StatusName: "Done", LaneKey: "status-12"},
		}},
	}

	lanes := buildPublicBoardLanes("status", columns, responseColumns)
	want := []string{"status-10", "status-11", "status-12"}
	if len(lanes) != len(want) {
		t.Fatalf("lanes = %+v, want %v", lanes, want)
	}
	for i, key := range want {
		if lanes[i].Key != key {
			t.Errorf("lane %d = %q, want %q", i, lanes[i].Key, key)
		}
	}
}

func TestBuildPublicBoardLanesProjectFallbackTitle(t *testing.T) {
	columns := []boardColumnInfo{{name: "Open"}}
	responseColumns := []publicColumn{
		{Items: []publicBoardCard{
			{ProjectName: "", LaneKey: "project-9"},
			{ProjectName: "Apollo", LaneKey: "project-7"},
		}},
	}

	lanes := buildPublicBoardLanes("project", columns, responseColumns)
	if len(lanes) != 2 {
		t.Fatalf("lanes = %+v, want two project lanes", lanes)
	}
	if lanes[0].Key != "project-7" || lanes[0].Title != "Apollo" {
		t.Errorf("first lane = %+v, want project-7 Apollo", lanes[0])
	}
	if lanes[1].Key != "project-9" || lanes[1].Title != "Project #9" {
		t.Errorf("second lane = %+v, want fallback title Project #9", lanes[1])
	}
}

func TestBuildPublicBoardLanesFlatGrouping(t *testing.T) {
	if lanes := buildPublicBoardLanes("", nil, nil); lanes != nil {
		t.Errorf("flat grouping returned lanes: %+v", lanes)
	}
}

func TestBuildCustomerBoardLanesNestsProjects(t *testing.T) {
	acmeID, betaID := 7, 8
	responseColumns := []publicColumn{
		{Items: []publicBoardCard{
			{LaneKey: "project-9", ProjectName: "Apollo", customerID: &acmeID, customerName: "Acme Co"},
			{LaneKey: "project-3", ProjectName: "Gemini", customerID: &acmeID, customerName: "Acme Co"},
			{LaneKey: "project-1", ProjectName: "Zephyr"},
			{LaneKey: "none", customerID: &betaID, customerName: "Beta GmbH"},
		}},
	}

	lanes := buildCustomerBoardLanes(responseColumns)
	if len(lanes) != 3 {
		t.Fatalf("containers = %+v, want Acme, Beta and no-customer", lanes)
	}

	// Customers sort by name; the no-customer container comes last.
	if lanes[0].Key != "customer-7" || lanes[0].Title != "Acme Co" {
		t.Errorf("first container = %+v, want customer-7 Acme Co", lanes[0])
	}
	if lanes[1].Key != "customer-8" || lanes[1].Title != "Beta GmbH" {
		t.Errorf("second container = %+v, want customer-8 Beta GmbH", lanes[1])
	}
	if lanes[2].Key != "customer-none" || lanes[2].Title != "No customer" {
		t.Errorf("last container = %+v, want the no-customer container", lanes[2])
	}

	acmeChildren := lanes[0].Children
	if len(acmeChildren) != 2 || acmeChildren[0].Key != "project-9" || acmeChildren[1].Key != "project-3" {
		t.Errorf("acme children = %+v, want Apollo then Gemini by title", acmeChildren)
	}
	if acmeChildren[0].Title != "Apollo" || acmeChildren[1].Title != "Gemini" {
		t.Errorf("acme child titles = %+v", acmeChildren)
	}

	if len(lanes[1].Children) != 1 || lanes[1].Children[0].Key != "none" || lanes[1].Children[0].Title != "No project" {
		t.Errorf("beta children = %+v, want the projectless lane", lanes[1].Children)
	}
	if len(lanes[2].Children) != 1 || lanes[2].Children[0].Key != "project-1" {
		t.Errorf("no-customer children = %+v, want project-1", lanes[2].Children)
	}
}

func TestPublicLaneKeyForCardCustomerUsesProjectKey(t *testing.T) {
	key := publicLaneKeyForCard("customer", &models.Item{}, &publicEffectiveProject{id: 4, name: "Apollo"})
	if key != "project-4" {
		t.Errorf("customer grouping lane key = %q, want project-4", key)
	}
	if key := publicLaneKeyForCard("customer", &models.Item{}, nil); key != "none" {
		t.Errorf("projectless lane key = %q, want none", key)
	}
}
