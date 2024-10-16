package sync

import (
	"sort"
	"time"

	"github.com/launchrctl/launchr"
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
	resources map[string]*Resource
	date      time.Time
}

// NewTimelineResourcesItem returns new instance of [TimelineResourcesItem]
func NewTimelineResourcesItem(version, commit string, date time.Time) *TimelineResourcesItem {
	return &TimelineResourcesItem{
		version:   version,
		commit:    commit,
		date:      date,
		resources: make(map[string]*Resource),
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
	i.resources[r.GetName()] = r
}

// GetResources returns [Resource] map of timeline.
func (i *TimelineResourcesItem) GetResources() map[string]*Resource {
	return i.resources
}

// Merge allows to merge other timeline item resources.
func (i *TimelineResourcesItem) Merge(item TimelineItem) {
	if r2, ok := item.(*TimelineResourcesItem); ok {
		for k, r := range r2.resources {
			if _, ok = i.resources[k]; !ok {
				i.resources[k] = r
			}
		}
	}
}

// Print outputs common item info.
func (i *TimelineResourcesItem) Print() {
	launchr.Term().Printfln("Version: %s, Date: %s, Commit: %s", i.GetVersion(), i.GetDate(), i.GetCommit())
	launchr.Term().Printf("Resource List:\n")
	for resource := range i.resources {
		launchr.Term().Printfln("- %s", resource)
	}
}

// TimelineVariablesItem implements TimelineItem interface and stores Variable map.
type TimelineVariablesItem struct {
	version   string
	commit    string
	variables map[string]*Variable
	date      time.Time
}

// NewTimelineVariablesItem returns new instance of [TimelineVariablesItem]
func NewTimelineVariablesItem(version, commit string, date time.Time) *TimelineVariablesItem {
	return &TimelineVariablesItem{
		version:   version,
		commit:    commit,
		date:      date,
		variables: make(map[string]*Variable),
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
	i.variables[v.GetName()] = v
}

// GetVariables returns [Variable] map of timeline.
func (i *TimelineVariablesItem) GetVariables() map[string]*Variable {
	return i.variables
}

// Merge allows to merge other timeline item variables.
func (i *TimelineVariablesItem) Merge(item TimelineItem) {
	if v2, ok := item.(*TimelineVariablesItem); ok {
		for k, v := range v2.variables {
			if _, ok = i.variables[k]; !ok {
				i.variables[k] = v
			}
		}
	}
}

// Print outputs common item info.
func (i *TimelineVariablesItem) Print() {
	launchr.Term().Printfln("Version: %s, Date: %s, Commit: %s", i.GetVersion(), i.GetDate(), i.GetCommit())
	launchr.Term().Printf("Variable List:\n")
	for variable := range i.variables {
		launchr.Term().Printfln("- %s", variable)
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
func SortTimeline(list []TimelineItem) {
	sort.Slice(list, func(i, j int) bool {
		return list[i].GetDate().Before(list[j].GetDate())
	})
}

// CreateTimeline returns fresh timeline slice.
func CreateTimeline() []TimelineItem {
	return make([]TimelineItem, 0)
}
