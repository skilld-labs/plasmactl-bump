package sync

import (
	"testing"
	"time"
)

var (
	t1 = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 = time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)
	t3 = time.Date(2000, 1, 3, 0, 0, 0, 0, time.UTC)
)

func newResItem(version string, date time.Time) *TimelineResourcesItem {
	return NewTimelineResourcesItem(version, "commit-"+version, date, nil)
}

func newVarItem(version string, date time.Time) *TimelineVariablesItem {
	return NewTimelineVariablesItem(version, "commit-"+version, date, nil)
}

func TestAddToTimeline(t *testing.T) {
	t.Run("new item is appended", func(t *testing.T) {
		list := CreateTimeline()
		item := newResItem("v1", t1)
		list = AddToTimeline(list, item)
		if len(list) != 1 {
			t.Fatalf("len = %d, want 1", len(list))
		}
	})

	t.Run("same version and date merges resources item", func(t *testing.T) {
		list := CreateTimeline()
		r1 := NewResource("interaction__skills__skill-a", "")
		r2 := NewResource("interaction__skills__skill-b", "")

		item1 := newResItem("v1", t1)
		item1.AddResource(r1)
		list = AddToTimeline(list, item1)

		item2 := newResItem("v1", t1)
		item2.AddResource(r2)
		list = AddToTimeline(list, item2)

		if len(list) != 1 {
			t.Fatalf("expected merge, len = %d", len(list))
		}
		res := list[0].(*TimelineResourcesItem).GetResources()
		if res.Len() != 2 {
			t.Errorf("merged resources count = %d, want 2", res.Len())
		}
	})

	t.Run("different version creates new item", func(t *testing.T) {
		list := CreateTimeline()
		list = AddToTimeline(list, newResItem("v1", t1))
		list = AddToTimeline(list, newResItem("v2", t1))
		if len(list) != 2 {
			t.Fatalf("len = %d, want 2", len(list))
		}
	})

	t.Run("different date creates new item", func(t *testing.T) {
		list := CreateTimeline()
		list = AddToTimeline(list, newResItem("v1", t1))
		list = AddToTimeline(list, newResItem("v1", t2))
		if len(list) != 2 {
			t.Fatalf("len = %d, want 2", len(list))
		}
	})

	t.Run("resources and variables with same version/date do not merge", func(t *testing.T) {
		list := CreateTimeline()
		list = AddToTimeline(list, newResItem("v1", t1))
		list = AddToTimeline(list, newVarItem("v1", t1))
		if len(list) != 2 {
			t.Fatalf("different types must not merge, len = %d", len(list))
		}
	})
}

func TestSortTimeline(t *testing.T) {
	tests := []struct {
		name      string
		order     string
		wantOrder []string
	}{
		{name: "ascending", order: SortAsc, wantOrder: []string{"v1", "v2", "v3"}},
		{name: "descending", order: SortDesc, wantOrder: []string{"v3", "v2", "v1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name+" by date", func(t *testing.T) {
			list := CreateTimeline()
			list = AddToTimeline(list, newResItem("v3", t3))
			list = AddToTimeline(list, newResItem("v1", t1))
			list = AddToTimeline(list, newResItem("v2", t2))
			SortTimeline(list, tt.order)
			for i, want := range tt.wantOrder {
				if list[i].GetVersion() != want {
					t.Errorf("[%d] = %q, want %q", i, list[i].GetVersion(), want)
				}
			}
		})
	}

	t.Run("equal dates asc: Variables before Resources", func(t *testing.T) {
		list := CreateTimeline()
		list = AddToTimeline(list, newResItem("res", t1))
		list = AddToTimeline(list, newVarItem("var", t1))
		SortTimeline(list, SortAsc)
		if _, ok := list[0].(*TimelineVariablesItem); !ok {
			t.Error("expected Variables first in asc sort with equal dates")
		}
	})

	t.Run("equal dates desc: Resources before Variables", func(t *testing.T) {
		list := CreateTimeline()
		list = AddToTimeline(list, newVarItem("var", t1))
		list = AddToTimeline(list, newResItem("res", t1))
		SortTimeline(list, SortDesc)
		if _, ok := list[0].(*TimelineResourcesItem); !ok {
			t.Error("expected Resources first in desc sort with equal dates")
		}
	})
}
