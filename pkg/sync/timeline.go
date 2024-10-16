package sync

import (
	"sort"
	"time"

	"github.com/launchrctl/launchr"
)

type TimelineItem interface {
	GetCommit() string
	GetVersion() string
	GetDate() time.Time
	Merge(item TimelineItem)
	Print()
}

type TimelineResourcesItem struct {
	version   string
	commit    string
	resources map[string]*Resource
	date      time.Time
}

func NewTimelineResourcesItem(version, commit string, date time.Time) *TimelineResourcesItem {
	return &TimelineResourcesItem{
		version:   version,
		commit:    commit,
		date:      date,
		resources: make(map[string]*Resource),
	}
}

func (i *TimelineResourcesItem) GetCommit() string {
	return i.commit
}

func (i *TimelineResourcesItem) GetVersion() string {
	return i.version
}

func (i *TimelineResourcesItem) GetDate() time.Time {
	return i.date
}

func (i *TimelineResourcesItem) AddResource(r *Resource) {
	i.resources[r.GetName()] = r
}

func (i *TimelineResourcesItem) GetResources() map[string]*Resource {
	return i.resources
}

func (i *TimelineResourcesItem) Merge(item TimelineItem) {
	if r2, ok := item.(*TimelineResourcesItem); ok {
		for k, r := range r2.resources {
			if _, ok = i.resources[k]; !ok {
				i.resources[k] = r
			}
		}
	}
}

func (i *TimelineResourcesItem) Print() {
	launchr.Term().Printfln("Version: %s, Date: %s, Commit: %s", i.GetVersion(), i.GetDate(), i.GetCommit())
	launchr.Term().Printf("Resource List:\n")
	for resource, _ := range i.resources {
		launchr.Term().Printfln("- %s", resource)
	}
}

type TimelineVariablesItem struct {
	version   string
	commit    string
	variables map[string]*Variable
	date      time.Time
}

func NewTimelineVariablesItem(version, commit string, date time.Time) *TimelineVariablesItem {
	return &TimelineVariablesItem{
		version:   version,
		commit:    commit,
		date:      date,
		variables: make(map[string]*Variable),
	}
}

func (i *TimelineVariablesItem) GetCommit() string {
	return i.commit
}

func (i *TimelineVariablesItem) GetVersion() string {
	return i.version
}

func (i *TimelineVariablesItem) GetDate() time.Time {
	return i.date
}

func (i *TimelineVariablesItem) AddVariable(v *Variable) {
	i.variables[v.GetName()] = v
}

func (i *TimelineVariablesItem) GetVariables() map[string]*Variable {
	return i.variables
}

func (i *TimelineVariablesItem) Merge(item TimelineItem) {
	if v2, ok := item.(*TimelineVariablesItem); ok {
		for k, v := range v2.variables {
			if _, ok = i.variables[k]; !ok {
				i.variables[k] = v
			}
		}
	}
}

func (i *TimelineVariablesItem) Print() {
	launchr.Term().Printfln("Version: %s, Date: %s, Commit: %s", i.GetVersion(), i.GetDate(), i.GetCommit())
	launchr.Term().Printf("Variable List:\n")
	for variable, _ := range i.variables {
		launchr.Term().Printfln("- %s", variable)
	}
}

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

func SortTimeline(list []TimelineItem) {
	sort.Slice(list, func(i, j int) bool {
		return list[i].GetDate().Before(list[j].GetDate())
	})
}

func CreateTimeline() []TimelineItem {
	return make([]TimelineItem, 0)
}
