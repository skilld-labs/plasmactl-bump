package plasmactlbump

import (
	"fmt"
	"sort"

	"github.com/launchrctl/launchr"
	"github.com/launchrctl/launchr/pkg/action"
	"github.com/skilld-labs/plasmactl-bump/v2/pkg/sync"
)

type dependenciesAction struct {
	action.WithLogger
	action.WithTerm
}

func (a *dependenciesAction) run(target, source string, toPath, showTree bool, depth int8) error {
	searchMrn := target
	_, errConvert := sync.ConvertMRNtoPath(searchMrn)

	if errConvert != nil {
		r := sync.BuildResourceFromPath(target, source)
		if r == nil {
			return fmt.Errorf("not valid resource %q", target)
		}

		searchMrn = r.GetName()
	}

	var header string
	if toPath {
		header, _ = sync.ConvertMRNtoPath(searchMrn)
	} else {
		header = searchMrn
	}

	inv, err := sync.NewInventory(source, a.Log())
	if err != nil {
		return err
	}
	parents := inv.GetRequiredByResources(searchMrn, depth)
	if len(parents) > 0 {
		a.Term().Info().Println("Dependent resources:")
		if showTree {
			var parentsTree forwardTree = inv.GetRequiredByMap()
			parentsTree.print(a.Term(), header, "", 1, depth, searchMrn, toPath)
		} else {
			a.printList(parents, toPath)
		}
	}

	children := inv.GetDependsOnResources(searchMrn, depth)
	if len(children) > 0 {
		a.Term().Info().Println("Dependencies:")
		if showTree {
			var childrenTree forwardTree = inv.GetDependsOnMap()
			childrenTree.print(a.Term(), header, "", 1, depth, searchMrn, toPath)
		} else {
			a.printList(children, toPath)
		}
	}

	return nil
}

func (a *dependenciesAction) printList(items map[string]bool, toPath bool) {
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	for _, item := range keys {
		res := item
		if toPath {
			res, _ = sync.ConvertMRNtoPath(res)
		}

		a.Term().Print(res + "\n")
	}
}

type forwardTree map[string]*sync.OrderedMap[bool]

func (t forwardTree) print(printer *launchr.Terminal, header, indent string, depth, limit int8, parent string, toPath bool) {
	if indent == "" {
		printer.Printfln(header)
	}

	if depth == limit {
		return
	}

	children, ok := t[parent]
	if !ok {
		return
	}

	keys := children.Keys()
	sort.Strings(keys)

	for i, node := range keys {
		isLast := i == len(keys)-1
		var newIndent, edge string

		if isLast {
			newIndent = indent + "    "
			edge = "└── "
		} else {
			newIndent = indent + "│   "
			edge = "├── "
		}

		value := node
		if toPath {
			value, _ = sync.ConvertMRNtoPath(value)
		}

		printer.Printfln(indent + edge + value)
		t.print(printer, "", newIndent, depth+1, limit, node, toPath)
	}
}
