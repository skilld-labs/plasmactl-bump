# plasmactl-bump

Update the version of components that were updated in the last commit.

### Excluded Files:

- README.md
- README.svg

---

**bump:** Updates the version of components that were modified after the last bump.

### Bump flow

1. Open the git repository.
2. Check if the latest commit is not a bump commit.
3. Collect a list of changed files until the previous bump commit is found (matching by author). If the `last` option is
   passed, only take changes from the last commit. Prepare a map of resource objects to update.
4. Get the short hash of the last commit.
5. Iterate through the resource map and update the version of each.

---

### `bump --sync` or Propagation

`bump` works closely with `plasmactl compose`. The purpose of `compose` is to build the platform using resources from
different places (domain repo, packages outside the repo).
The purpose of `bump --sync` is to propagate the versions of changed resources to dependant resources in current build (
post composition) while preserving history of earlier propagated versions.

### Overall Propagation Workflow:

- Search and download the artifact to compare the build to.
- Find the list of different files between the build and the artifact.
- Identify list of resources which version should be propagated.
- Build propagation map.
- Update resources in build dir.

#### Prerequisites:

- It is important to use `bump --sync` after a fresh `compose`. Otherwise, an incorrect result or error may occur.

### Detailed Propagation Workflow:

1. **Search and download the artifact to compare the build to:**

- Open the git repository.
- Search for the first bump commit after HEAD and use it's short sha (7 characters) to compute artifact name to
  retrieve.
- Check if the artifact was previously downloaded and stored in the `.compose/artifacts` directory. If it exists, use it
  as local cache instead of re-downloading it.
- Attempt to download the artifact with the name `repo_name-commit_hash_7_symbols-plasma-src.tar.gz` from the
  repository.
- If artifact doesn't exist with that name, recursively look for earlier bump commits.
- Unarchive the artifact into the comparison directory (usually `.compose/comparison-artifact`).

> You can override the artifact commit with the `override` option, which bypasses the git history search and directly
> attempts to download the artifact.

2. **Find the list of different files between the build and the artifact:**

- Compare the files in the build directory to the files in the artifact directory.
- If two files differ, their paths are added to the list of updated items.
- If file doesn't exist in artifact, path is added to updates items.
- If file exists in artifact, but not in the build, path is added to updates items.

**Excluded Subdirectories and Files:**

- `.git`
- `.compose`
- `.plasmactl`
- `.gitlab-ci.yml`
- `ansible_collections`
- `scripts/ci/.gitlab-ci.platform.yaml`
- `venv`
- `__pycache__`

3. **Identify list of resources which version should be propagated:**

- Prepare list of resources names per source (domain + packages names), if resource exists in several sources, identify
  origin of composed resource (
  see [Syncing Resource Versions Across Namespaces](#syncing-resource-versions-across-sources)) and remove duplicates.
- Convert list of changes files to list of resources. if filepath matches [resource criteria](#resource-criteria),
  resource object will be created and added to list of changed resources.
- Initialize [timeline](#timeline).
- Iterate through modified resources, find source of resource (domain or package) and populate timeline .
  see [Iterating Through Variables](#iterating-through-resources)
- Find in list of modified files `group_vars.yaml` and `vault.yaml` files.
- Iterate through variables files and populate timeline list with changed variables.
  see [Iterating Through Variables](#iterating-through-variables)

#### Resource criteria

> @todo add

#### Syncing Resource Versions Across Sources:

Next, we filter out resources that exist in multiple namespaces.
`compose` handles this via merge strategies, but `sync` compares the build version with the namespace versions to select
the correct resource.
Once sorted, we iterate over the list of resources that differ between the build and the artifact.

> @TODO: Temporary solution, better handling of compose merge strategies should be implemented.

Once different files are listed, filter out platform resources (to be covered in more detail).
With the help of the inventory helper (which checks resources and variables), resources are divided into namespaces.
These namespaces are split into **domain** and **packages**. While the domain is always static, packages come
from `plasma-compose`.

#### Timeline

> @todo update

To propagate, we first search for the commit where the resource version was set. This commit is added to a special list
of objects called timeline.
Each timeline object contains the commit date, a list of variables to propagate, or a list of resources.
The timeline is sorted by date to maintain the correct version sequence as if propagation was applied after each change.

After iterating through the timeline (chronologically sorted) and applying dependent resource versions, the latest state
is achieved.
At this point, we update the real resource in the build directory.

- If the resource version matches the propagation map, it’s skipped (happens in case when both parent and child
  resources were bumped. No need to propagate parent version to child if they already have identical new version).
- Otherwise, the version is set as `resource_version-propagated_version`.

> @todo add

#### Iterating Through Resources:

During this phase, the following rules apply:

- If a resource not present in build, it’s skipped. The developer should handle dependencies manually and bump related
  resources (in this case dependent resource will be considered as `updated` and require propagation).
- If a resource is new, add it to the propagation list.
- If a resource differs between the build and the artifact, compare versions. If the base version (e.g., base,
  base-propagation_suffix) differs, add it to the propagation list. Otherwise, copy the resource version from the
  artifact to preserve history of earlier propagated versions.
- If entry with the same date and commit existed before, merge entries.

#### Iterating Through Variables:

During this phase, the following rules apply:

- If variables file is new, find all new variables and commit where they were added, create new timeline entry per
  commit and add them to timeline.
- If variables file was deleted, find all deleted variables and commit where they were deleted, create new timeline
  entry and add to timeline.
- If variables file exists, but one or several variables were changed, search commits where these changes were done,
  create new timeline entry per commit, add them to timeline.
- If entry with the same date and commit existed before, merge entries.


4. **Build propagation map:**

- Chronologically sort timeline.
- Iterate each timeline entry.
- If entry has resources or variables, find list of dependent resources and store for each of them version of timeline
  entry (propagate).
- If resource existed in propagation map, it will be overridden by next timeline entry version.


5. **Update resources in build dir:**

- Iterate each resource in propagation map.
- Build new version for resource, which consists of resource original and propagated versions, final result will look
  like `resource_version-propagated_version`.
- Update each resource meta to store new version.

---

### Acceptance criteria

#### Case 1: propagation with no changes

As a developer:

- Clone an existing git repo.
- Run `plasmactl bump` to update the resource version during developments.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned` to prepare the build.
- Run `plasmactl bump --sync` to propagate versions of updated resources to dependent resources.

**Expected Outcome:**  
No bumping occurs because no resource was updated. No propagation occurs because there is no difference between current
build and comparison artifact.
However, all propagated versions from the artifact should be copied to current build if the base version doesn't match
between the build and artifact.

Test status: **OK**

---

#### Case 2: Updated resources in domain repo

As a developer:

- Update an existing resource in domain repo (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version during developments
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync` to propagate the new version to dependent resources.

**Expected Outcome:**  
The new version from the bump (e.g., 1111111111111) should propagate to all dependent resources
as `resource_version-1111111111111`.

Test status: **OK**

Additional notes:

- What if the developer didn't bump the version before propagation?
- Should an error occur if a non-bumped or uncommitted version is set?

---

#### Case 3: Removed resources in domain repo

As a developer:

- Remove a resource.
- Run `plasmactl bump` to update the resource version during developments.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync` to propagate the changes.

**Expected Outcome:**  
No bumping or propagation. The developer should manually remove any dependencies before the bump.

Test status: **?**

---

#### Case 4: Added resources in domain repo

As a developer:

- Add a new resource.
- Run `plasmactl bump` to update the resource version during developments.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**  
The new resource is bumped, but nothing else is propagated. Propagation occurs if the resource is overridden from
another namespace (e.g., domain or package).

Test status: **OK**

---

#### Case 5: Added resources in a package

---

#### Case 6: Updated resources in a package

---

#### Case 7: Removed resources from a package

---

#### Case 8: Updated variable in an existing group_vars/vault in domain repo

As a developer:

- Update a variable in `group_vars` or `vault.yaml`.
- Run `plasmactl bump` to update the resource version during developments.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**  
No bumping occurs, but the variable change should propagate to all dependent resources, using commit where change
happened.

Test status: **?**

---

#### Case 9:  Removed variable in an existing group_vars/vault in domain repo

As a developer:

- Update a variable in `group_vars` or `vault.yaml`.
- Run `plasmactl bump` to update the resource version during developments.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**  
No bumping occurs, but the variable change should propagate to all dependent resources, using commit where change
happened.

Test status: **?**

---

#### Case 10: Added variable in an existing group_vars/vault in domain repo

As a developer:

- Update a variable in `group_vars` or `vault.yaml`.
- Run `plasmactl bump` to update the resource version during developments.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**  
No bumping occurs, but the variable change should propagate to all dependent resources, using commit where change
happened.

Test status: **?**

---

#### Case 11: Added group_vars/vault file in domain repo

As a developer:

- Create `group_vars` or `vault.yaml` files.
- Run `plasmactl bump` to update the resource version during developments.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**  
No bumping, but the file creation commit should be propagated to dependent resources of each variable in new file.

Test status: **?**

---

#### Case 12: Removed group_vars/vault file in domain repo

As a developer:

- Remove `group_vars` or `vault.yaml` files.
- Run `plasmactl bump` to update the resource version during developments.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**  
No bumping, but the file deletion commit should be propagated to dependent resources of each deleted variable.

Test status: **OK**

---

### Advanced criteria with examples

Package repo:

#### Case 13: Successive updates of different resources in domain repo (with no dependency to each other)

As a developer:

- Update several existing resources in domain repo (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version during developments.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**

- Result of previous propagation is copied to preserve propagation history.
- Updated resources in domain repository are bumped.
- All dependent resources of bumped resources are updated.

Test status: **?**

---

#### Case 14: Successive updates of different resources in package (with no dependency to each other)

As a developer:

- Update several existing resources in package repo (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resources versions during developments.
- Update `plasma-compose.yaml` in domain repo with new tag or branch.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**

- Result of previous propagation is copied to preserve propagation history.
- Updated resources in package repository are bumped.
- All dependent resources of bumped resources are updated.

Test status: **?**

---

#### Case 15: Successive updates of different resources in domain repo then package (with no dependency to each other)

As a developer:

- Update several existing resources in domain repo (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version during developments.
- Update several existing resources in package repo (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version during developments.
- Update `plasma-compose.yaml` in domain repo with new tag or branch.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**

- Result of previous propagation is copied to preserve propagation history.
- Updated resources in domain repository are bumped.
- Updated resources in package repository are bumped.
- All dependent resources of bumped domain resources are updated.
- All dependent resources of bumped package resources are updated.

> @TODO add an example?

Test status: **?**

---

#### Case 16: Successive updates of different resources in package repo then domain repo (with no dependency to each other)

As a developer:

- Update several existing resources in package repo (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version during developments.
- Update `plasma-compose.yaml` in domain repo with new tag or branch.
- Update several existing resources in domain repo (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version during developments.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**

- Result of previous propagation is copied to preserve propagation history.
- Updated resources in domain repository are bumped.
- Updated resources in package repository are bumped.
- All dependent resources of bumped package resources are updated.
- All dependent resources of bumped domain resources are updated.

> @TODO add an example?

Test status: **?**

---

#### Case 17: Successive updates of resources with low dep in domain repo and dependants in package repo (with dependency to each other)

Example state of resources:

```
 library-from-domain - ver1
 └── function-from-package - ver1
   └── skill-from-package - ver1
     └── flow-from-package - ver1
       └── executor-from-domain - ver1 
```

As a developer:

- Update low resource `library-from-domain` in domain repository (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version during developments.
- Update resource `skill-from-package` in package repo (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version during developments.
- Update `plasma-compose.yaml` in domain repo with new tag or branch.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**

- Result of previous propagation is copied to preserve propagation history.
- Updated resource `library-from-domain` in domain repository is bumped and receives new version (`bump_commit_1`)
- Updated resource `skill-from-package` in package repository is bumped and receives new version (`bump_commit_2`)
- All dependent resources of bumped domain resources are updated.

``` 
Intermediate result of propagating versions.

 library-from-domain - bump_commit_1
 └── function-from-package - ver1-bump_commit_1
   └── skill-from-package - ver1-bump_commit_1
     └── flow-from-package - ver1-bump_commit_1
       └── executor-from-domain - ver1-bump_commit_1
```

- All dependent resources of bumped package resources are updated.

```
Final result of propagating versions.

 library-from-domain - bump_commit_1
 └── function-from-package - ver1-bump_commit_1
   └── skill-from-package - bump_commit_2
     └── flow-from-package - ver1-bump_commit_2
       └── executor-from-domain - ver1-bump_commit_2
```

Test status: **?**

---

#### Case 18: Successive updates of resources with low dep in package repo and dependants in domain rep (with dependency to each other)

Example state of resources:

```
 library-from-package - ver1
 └── function-from-domain - ver1
   └── skill-from-domain - ver1
     └── flow-from-domain - ver1
       └── executor-from-domain - ver1   
```

As a developer:

- Update low resource `library-from-package` in package repository (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version during developments.
- Update resource `skill-from-domain` in package repository (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version during developments.
- Update `plasma-compose.yaml` in domain repository with new tag or branch.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**

- Result of previous propagation is copied to preserve propagation history.
- Updated resource `library-from-package` in domain repository is bumped and receives new version (`bump_commit_1`)
- Updated resource `skill-from-domain` in package repository is bumped and receives new version (`bump_commit_2`)
- All dependent resources of bumped domain resources are updated.

``` 
Intermediate result of propagating versions.

 library-from-package - bump_commit_1
 └── function-from-domain - ver1-bump_commit_1
   └── skill-from-domain - ver1-bump_commit_1
     └── flow-from-domain - ver1-bump_commit_1
       └── executor-from-domain - ver1-bump_commit_1
```

- All dependent resources of bumped package resources are updated.

```
Final result of propagating versions.

 library-from-package - bump_commit_1
 └── function-from-domain - ver1-bump_commit_1
   └── skill-from-domain - bump_commit_2
     └── flow-from-domain - ver1-bump_commit_2
       └── executor-from-domain - ver1-bump_commit_2
```

Test status: **?**

---

#### Case 19: Successive updates of resources with low dep in package repository and dependants in domain rep (with dependency to each other) then updated variable in an existing group_vars/vault in domain repo

Resources for example:

```
group_vars:
- test_variable

test_variable used in skill-from-domain


 library-from-package - ver1
 └── function-from-domain - ver1
   └── skill-from-domain - ver1
     └── flow-from-domain - ver1
       └── executor-from-domain - ver1   
```

As a developer:

- Update low resource `library-from-package` in package repository (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version during developments.
- Update resource `skill-from-domain` in domain repository (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version during developments.
- Update variable `test_variable` in group_vars file.
- Update `plasma-compose.yaml` in domain repository with new tag or branch.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**

- Result of previous propagation is copied to preserve propagation history.
- Updated resource `library-from-package` in domain repository is bumped and receives new version (`bump_commit_1`).
- Updated resource `skill-from-domain` in package repository is bumped and receives new version (`bump_commit_2`).
- Variable update is detected, commit where variable was changed is `variable_change_commit`.
- All dependent resources of bumped package resources are updated.

``` 
Intermediate result of propagating versions 1.

 library-from-package - bump_commit_1
 └── function-from-domain - ver1-bump_commit_1
   └── skill-from-domain - ver1-bump_commit_1
     └── flow-from-domain - ver1-bump_commit_1
       └── executor-from-domain - ver1-bump_commit_1
```

- All dependent resources of bumped domain resources are updated.

```
Intermediate result of propagating versions 2.

 library-from-package - bump_commit_1
 └── function-from-domain - ver1-bump_commit_1
   └── skill-from-domain - bump_commit_2
     └── flow-from-domain - ver1-bump_commit_2
       └── executor-from-domain - ver1-bump_commit_2
```

- All dependent resources of changed variable are updated.

```
Final result of propagating versions.

 library-from-package - bump_commit_1
 └── function-from-domain - ver1-bump_commit_1
   └── skill-from-domain - bump_commit_2-variable_change_commit
     └── flow-from-domain - ver1-variable_change_commit
       └── executor-from-domain - ver1-variable_change_commit
```

Test status: **?**

---
