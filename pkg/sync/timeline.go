package sync

import (
	"sort"
	"time"

	"github.com/launchrctl/launchr"
)

const (
	SortAsc  = "asc"  // SortAsc const.
	SortDesc = "desc" // SortDesc const.
)

// TimelineItem is interface for storing commit, date and version of propagated items.
// Storing such items in slice allows us to propagate items in the same order they were changed.
type TimelineItem interface {
	GetCommit() string
	GetVersion() string
	GetDate() time.Time
	Merge(item TimelineItem)
	Print()
}

// TimelineResourcesItem implements TimelineItem interface and stores Resource map.
type TimelineResourcesItem struct {
	version   string
	commit    string
	resources *OrderedMap[*Resource]
	date      time.Time
}

// NewTimelineResourcesItem returns new instance of [TimelineResourcesItem]
func NewTimelineResourcesItem(version, commit string, date time.Time) *TimelineResourcesItem {
	return &TimelineResourcesItem{
		version:   version,
		commit:    commit,
		date:      date,
		resources: NewOrderedMap[*Resource](),
	}
}

// GetCommit returns timeline item commit.
func (i *TimelineResourcesItem) GetCommit() string {
	return i.commit
}

// GetVersion returns timeline item version to propagate.
func (i *TimelineResourcesItem) GetVersion() string {
	return i.version
}

// GetDate returns timeline item date.
func (i *TimelineResourcesItem) GetDate() time.Time {
	return i.date
}

// AddResource pushes [Resource] into timeline item.
func (i *TimelineResourcesItem) AddResource(r *Resource) {
	i.resources.Set(r.GetName(), r)
}

// GetResources returns [Resource] map of timeline.
func (i *TimelineResourcesItem) GetResources() *OrderedMap[*Resource] {
	return i.resources
}

// Merge allows to merge other timeline item resources.
func (i *TimelineResourcesItem) Merge(item TimelineItem) {
	if r2, ok := item.(*TimelineResourcesItem); ok {
		for _, key := range r2.resources.Keys() {
			_, exists := i.resources.Get(key)
			if exists {
				continue
			}

			itemRes, exists := r2.resources.Get(key)
			if !exists {
				continue
			}

			i.resources.Set(key, itemRes)
		}
	}
}

// Print outputs common item info.
func (i *TimelineResourcesItem) Print() {
	launchr.Term().Printfln("Version: %s, Date: %s, Commit: %s", i.GetVersion(), i.GetDate(), i.GetCommit())
	launchr.Term().Printf("Resource List:\n")
	for _, key := range i.resources.Keys() {
		v, ok := i.resources.Get(key)
		if !ok {
			continue
		}
		launchr.Term().Printfln("- %s", v.GetName())
	}
}

// TimelineVariablesItem implements TimelineItem interface and stores Variable map.
type TimelineVariablesItem struct {
	version   string
	commit    string
	variables *OrderedMap[*Variable]
	date      time.Time
}

// NewTimelineVariablesItem returns new instance of [TimelineVariablesItem]
func NewTimelineVariablesItem(version, commit string, date time.Time) *TimelineVariablesItem {
	return &TimelineVariablesItem{
		version:   version,
		commit:    commit,
		date:      date,
		variables: NewOrderedMap[*Variable](),
	}
}

// GetCommit returns timeline item commit.
func (i *TimelineVariablesItem) GetCommit() string {
	return i.commit
}

// GetVersion returns timeline item version to propagate.
func (i *TimelineVariablesItem) GetVersion() string {
	return i.version
}

// GetDate returns timeline item date.
func (i *TimelineVariablesItem) GetDate() time.Time {
	return i.date
}

// AddVariable pushes [Variable] into timeline item.
func (i *TimelineVariablesItem) AddVariable(v *Variable) {
	i.variables.Set(v.GetName(), v)
}

// GetVariables returns [Variable] map of timeline.
func (i *TimelineVariablesItem) GetVariables() *OrderedMap[*Variable] {
	return i.variables
}

// Merge allows to merge other timeline item variables.
func (i *TimelineVariablesItem) Merge(item TimelineItem) {
	if v2, ok := item.(*TimelineVariablesItem); ok {
		for _, key := range v2.variables.Keys() {
			_, exists := i.variables.Get(key)
			if exists {
				continue
			}

			itemVar, exists := v2.variables.Get(key)
			if !exists {
				continue
			}

			i.variables.Set(key, itemVar)
		}
	}
}

// Print outputs common item info.
func (i *TimelineVariablesItem) Print() {
	launchr.Term().Printfln("Version: %s, Date: %s, Commit: %s", i.GetVersion(), i.GetDate(), i.GetCommit())
	launchr.Term().Printf("Variable List:\n")
	for _, key := range i.variables.Keys() {
		v, ok := i.variables.Get(key)
		if !ok {
			continue
		}
		launchr.Term().Printfln("- %s", v.GetName())
	}
}

// AddToTimeline inserts items into timeline slice.
func AddToTimeline(list []TimelineItem, item TimelineItem) []TimelineItem {
	for _, i := range list {
		switch i.(type) {
		case *TimelineVariablesItem:
			if _, ok := item.(*TimelineVariablesItem); !ok {
				continue
			}
		case *TimelineResourcesItem:
			if _, ok := item.(*TimelineResourcesItem); !ok {
				continue
			}
		default:
			continue
		}

		if i.GetVersion() == item.GetVersion() && i.GetDate().Equal(item.GetDate()) {
			i.Merge(item)
			return list
		}
	}

	return append(list, item)
}

// SortTimeline sorts timeline items in slice.
func SortTimeline(list []TimelineItem, order string) {
	sort.Slice(list, func(i, j int) bool {
		dateI := list[i].GetDate()
		dateJ := list[j].GetDate()

		// Determine the date comparison based on the order
		if !dateI.Equal(dateJ) {
			if order == SortAsc {
				return dateI.Before(dateJ)
			}
			return dateI.After(dateJ)
		}

		// If dates are the same, prioritize by type
		switch list[i].(type) {
		case *TimelineVariablesItem:
			switch list[j].(type) {
			case *TimelineVariablesItem:
				// Both are Variables, maintain current order
				return false
			case *TimelineResourcesItem:
				// Variables come before Resources if asc, after if desc
				return order == SortAsc
			default:
				// Variables come before unknown types
				return true
			}
		case *TimelineResourcesItem:
			switch list[j].(type) {
			case *TimelineVariablesItem:
				// Resources come after Variables if asc, before if desc
				return order == SortDesc
			case *TimelineResourcesItem:
				// Both are Resources, maintain current order
				return false
			default:
				// Resources come before unknown types
				return true
			}
		default:
			switch list[j].(type) {
			case *TimelineVariablesItem, *TimelineResourcesItem:
				// Unknown types come after Variables and Resources
				return false
			default:
				// Maintain current order for unknown types
				return false
			}
		}
	})
}

// CreateTimeline returns fresh timeline slice.
func CreateTimeline() []TimelineItem {
	return make([]TimelineItem, 0)
}
