# plasmactl-bump
Update the version of components that were updated in the last commit.

### Excluded Files:
- README.md
- README.svg

---

### Work in Progress (WIP)

**bump:** Updates the version of components that were modified after the last bump.  
**Current bump logic:**
1. Open the git repository.
2. Check if the latest commit is not a bump commit.
3. Collect a list of changed files until the previous bump commit is found (matching by author??). If the `last` option is passed, only take changes from the last commit. Prepare a map of resource objects to update.
4. Get the short hash of the last commit.
5. Iterate through the resource map and update the version of each.

---

### `bump --sync` or Propagation

When resources are decoupled and stored in packages, there’s a need to preserve versions and maintain the current state of resources.
`bump` works closely with `plasmactl compose`. The purpose of `compose` is to build the platform using resources from different places (domain repo, packages outside the repo).
The purpose of `bump --sync` is to propagate the versions of a build to the current state.

**How is it achieved?**

#### Prerequisites:
- It is important to use `bump --sync` after a fresh `compose`. Otherwise, an incorrect result or error may occur.

### Overall Propagation Workflow:
1. **Search and download the artifact to compare the build to:**
  - Open the git repository.
  - Search for the first bump commit after `HEAD`.
  - Check if the artifact was previously downloaded and stored in the `.compose/artifacts` directory. If it exists, skip to step 5.
  - Attempt to download the artifact with the name `repo_name-commit_hash_7_symbols-plasma-src.tar.gz` from the repository.
  - Unarchive the artifact into the comparison directory (usually `.compose/comparison-artifact`).

   > You can override the artifact commit with the `override` option, which bypasses the git history search and directly attempts to download the artifact.

2. **Find the list of different files between the build and the artifact:**
  - Compare the files in the build directory to the files in the artifact directory.
  - If two files differ, their paths are added to the list of updated items.

   **Excluded Subdirectories and Files:**
  - `.git`
  - `.compose`
  - `.plasmactl`
  - `.gitlab-ci.yml`
  - `ansible_collections`
  - `scripts/ci/.gitlab-ci.platform.yaml`
  - `venv`

   > @TODO: Include new files in the build directory but not in the artifact and note deleted files from the artifact.

---

### Propagating Versions

Once you have the list of different files, filter out platform resources (to be covered in more detail). With the help of the inventory helper (which checks resources and variables), resources are divided into namespaces. These namespaces are split into **domain** and **packages**. While the domain is always static, packages come from `plasma-compose`.

#### Syncing Resource Versions Across Namespaces:
Next, we filter out resources that exist in multiple namespaces. `compose` handles this via merge strategies, but `sync` compares the build version with the namespace versions to select the correct resource. Once sorted, we iterate over the list of resources that differ between the build and the artifact.

> @TODO: (Temporary) Should we include additional technical details here?

### Iterating Through Resources:
During this phase, the following rules apply:
- If a resource is deleted from the build, it’s skipped. The developer should handle dependencies manually and bump related resources.
- If a resource is new, add it to the propagation list.
- If a resource differs between the build and the artifact, compare versions. If the base version (e.g., base, base-propagation_suffix) differs, add it to the propagation list. Otherwise, copy the resource version from the artifact.

The same process is applied to variables by comparing `group_var` and `vault.yaml` files. Deleted, changed, or added variables will also be propagated.

#### Propagation Map and Timeline:
To propagate, we first search for the commit where the resource version was set, and a special list of objects called `timeline` (temporary name) is created. Each timeline object contains the commit date, a list of variables to propagate, or a list of resources. The timeline is sorted by date to maintain the correct version sequence as if propagation was applied after each change.

After iterating through the timeline and applying dependent resource versions, the latest state is achieved. At this point, we update the real resource in the build directory.

- If the resource version matches the propagation map, it’s skipped.
- Otherwise, the version is set as `resource_version-propagated_version`.

---

### Common Propagation Cases (WIP — Convert to Tests?)

#### Case 0:
As a developer:
- Clone an existing git repo.
- Run `plasmactl bump` to update the resource version.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned` to prepare the build.
- Run `plasmactl bump --sync` to propagate versions of updated resources to dependent resources.

**Expected Outcome:**  
No bumping or propagation occurs. However, the command should copy propagated versions from the artifact if the base version doesn’t match between the build and artifact.

Test status: **OK**

---

#### Case 1:
As a developer:
- Update an existing resource (e.g., change a template, task, dependency).
- Run `plasmactl bump` to update the resource version.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync` to propagate the new version to dependent resources.

**Expected Outcome:**  
The new version from the bump (e.g., 1111111111111) should propagate to all dependent resources as `resource_version-1111111111111`.

Test status: **OK**

Additional notes:
- What if the developer didn't bump the version before propagation?
- Should an error occur if a non-bumped or uncommitted version is set?

---

#### Case 2:
As a developer:
- Remove a resource.
- Run `plasmactl bump`.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync` to propagate the changes.

**Expected Outcome:**  
No bumping or propagation. The developer should manually remove any dependencies before the bump.

Test status: **?**

---

#### Case 3:
As a developer:
- Add a new resource.
- Run `plasmactl bump`.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**  
The new resource is bumped, but nothing else is propagated. Propagation occurs if the resource is overridden from another namespace (e.g., domain or package).

Test status: **OK**

---

#### Case 4:
Test same steps for packages; expected results should be the same.

---

#### Case 5:
As a developer:
- Update a variable in `group_vars` or `vault.yaml`.
- Run `plasmactl bump`.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**  
No bumping occurs, but the variable change should propagate to all dependent resources

Test status: **?**

---

#### Case 6:
Should this case be included?
- Create `group_vars` or `vault.yaml` files.
- Run `plasmactl bump`.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**  
No bumping, but the file creation commit should propagate to dependent resources.

Test status: **?**

---

#### Case 7:
Should this case be included?
- Remove `group_vars` or `vault.yaml` files.
- Run `plasmactl bump`.
- Run `plasmactl compose --conflicts-verbosity --skip-not-versioned`.
- Run `plasmactl bump --sync`.

**Expected Outcome:**  
No bumping. The commit where the file was deleted should propagate to dependent resources.

Test status: **OK**

---

#### Additional Cases:
Add more specific cases, such as working with a chain of commits?

---