package plasmactlbump

//func collectVarsFilesCommits(r *git.Repository, varsFiles []string) (map[string][]*hashStruct, error) {
//	temp := make(map[string][]*hashStruct)
//	for _, path := range varsFiles {
//		_, ok := temp[path]
//		if !ok {
//			temp[path] = []*hashStruct{}
//		}
//	}
//
//	ref, err := r.Head()
//	if err != nil {
//		return temp, err
//	}
//
//	// start from the latest commit and iterate to the past
//	cIter, err := r.Log(&git.LogOptions{From: ref.Hash()})
//	if err != nil {
//		return temp, err
//	}
//
//	_ = cIter.ForEach(func(c *object.Commit) error {
//		// Get the tree of the current commit
//		tree, err := c.Tree()
//		if err != nil {
//			return fmt.Errorf("error getting tree for commit %s: %v", c.Hash, err)
//		}
//
//		// Get the parent tree (if it exists)
//		var parentTree *object.Tree
//		if c.NumParents() > 0 {
//			parentCommit, err := c.Parents().Next()
//			if err != nil {
//				return fmt.Errorf("error getting parent commit: %v", err)
//			}
//			parentTree, err = parentCommit.Tree()
//			if err != nil {
//				return fmt.Errorf("error getting parent tree: %v", err)
//			}
//		}
//
//		// Get the changes between the two trees
//		if parentTree != nil {
//			changes, err := object.DiffTree(parentTree, tree)
//			if err != nil {
//				return fmt.Errorf("error diffing trees: %v", err)
//			}
//
//			hs := &hashStruct{
//				hash:     c.Hash.String(),
//				author:   c.Author.Name,
//				hashTime: c.Author.When,
//			}
//
//			for _, change := range changes {
//				action, err := change.Action()
//				if err != nil {
//					return fmt.Errorf("error getting change action: %v", err)
//				}
//				var path string
//
//				switch action {
//				case merkletrie.Delete:
//					path = change.From.Name
//				case merkletrie.Modify:
//					path = change.From.Name
//				case merkletrie.Insert:
//					path = change.To.Name
//				}
//
//				if _, ok := temp[path]; ok {
//					temp[path] = append(temp[path], hs)
//				}
//			}
//		}
//
//		return nil
//	})
//
//	tests := 0
//	for k, v := range temp {
//		tests = tests + 1
//		launchr.Term().Info().Printfln(k)
//		for _, hs := range v {
//			launchr.Term().Warning().Printfln("%s %s %s", hs.hashTime, hs.author, hs.hash)
//		}
//
//	}
//
//	return temp, nil
//}
