package state

import (
	"fmt"
	"github.com/roboll/helmfile/helmexec"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

type ChartMeta struct {
	Name string `yaml:"name"`
}

type unresolvedChartDependency struct {
	// ChartName identifies the dependant chart. In Helmfile, ChartName for `chart: stable/envoy` would be just `envoy`.
	// It can't be collided with other charts referenced in the same helmfile spec.
	// That is, collocating `chart: incubator/foo` and `chart: stable/foo` isn't allowed. Name them differently for a work-around.
	ChartName string `yaml:"name"`
	// Repository contains the URL for the helm chart repository that hosts the chart identified by ChartName
	Repository string `yaml:"repository"`
	// VersionConstraint is the version constraint of the dependent chart. "*" means the latest version.
	VersionConstraint string `yaml:"version"`
}

type ResolvedChartDependency struct {
	// ChartName identifies the dependant chart. In Helmfile, ChartName for `chart: stable/envoy` would be just `envoy`.
	// It can't be collided with other charts referenced in the same helmfile spec.
	// That is, collocating `chart: incubator/foo` and `chart: stable/foo` isn't allowed. Name them differently for a work-around.
	ChartName string `yaml:"name"`
	// Repository contains the URL for the helm chart repository that hosts the chart identified by ChartName
	Repository string `yaml:"repository"`
	// Version is the version number of the dependent chart.
	// In the context of helmfile this can be omitted. When omitted, it is considered `*` which results helm/helmfile fetching the latest version.
	Version string `yaml:"version"`
}

// StatePackage is for packaging your helmfile state file along with its dependencies.
// The only type of dependency currently supported is `chart`.
// It is transient and generated on demand while resolving dependencies, and automatically removed afterwards.
type StatePackage struct {
	// name is the name of the package.
	// Usually this is the "basename" of the helmfile state file, e.g. `helmfile.2` when the state file is named `helmfile.2.yaml`, `helmfille.2.gotmpl`, or `helmfile.2.yaml.gotmpl`.
	name string

	chartDependencies map[string]unresolvedChartDependency
}

type UnresolvedDependencies struct {
	deps map[string]unresolvedChartDependency
}

type ChartRequirements struct {
	UnresolvedDependencies []unresolvedChartDependency `yaml:"dependencies"`
}

type ChartLockedRequirements struct {
	ResolvedDependencies []ResolvedChartDependency `yaml:"dependencies"`
}

func (d *UnresolvedDependencies) Add(chart, url, versionConstraint string) error {
	dep := unresolvedChartDependency{
		ChartName:         chart,
		Repository:        url,
		VersionConstraint: versionConstraint,
	}
	return d.add(dep)
}

func (d *UnresolvedDependencies) add(dep unresolvedChartDependency) error {
	existing, exists := d.deps[dep.ChartName]
	if exists && (existing.Repository != dep.Repository || existing.VersionConstraint != dep.VersionConstraint) {
		return fmt.Errorf("duplicate chart dependency \"%s\". you can't have two or more charts with the same name but with different urls or versions: existing=%v, new=%v", dep.ChartName, existing, dep)
	}
	d.deps[dep.ChartName] = dep
	return nil
}

func (d *UnresolvedDependencies) ToChartRequirements() *ChartRequirements {
	deps := []unresolvedChartDependency{}

	for _, d := range d.deps {
		if d.VersionConstraint == "" {
			d.VersionConstraint = "*"
		}
		deps = append(deps, d)
	}

	return &ChartRequirements{UnresolvedDependencies: deps}
}

type ResolvedDependencies struct {
	deps map[string]ResolvedChartDependency
}

func (d *ResolvedDependencies) add(dep ResolvedChartDependency) error {
	_, exists := d.deps[dep.ChartName]
	if exists {
		return fmt.Errorf("duplicate chart dependency \"%s\"", dep.ChartName)
	}
	d.deps[dep.ChartName] = dep
	return nil
}

func (d *ResolvedDependencies) Get(chart string) (string, error) {
	dep, exists := d.deps[chart]
	if !exists {
		return "", fmt.Errorf("no resolved dependency found for \"%s\"", chart)
	}
	return dep.Version, nil
}

func resolveRemoteChart(repoAndChart string) (string, string, bool) {
	parts := strings.Split(repoAndChart, "/")
	if isLocalChart(repoAndChart) {
		return "", "", false
	}
	if len(parts) != 2 {
		panic(fmt.Sprintf("unsupported format of chart name: %s", repoAndChart))
	}

	repo := parts[0]
	chart := parts[1]

	return repo, chart, true
}

func (st *HelmState) mergeLockedDependencies() (*HelmState, error) {
	filename, unresolved, err := getUnresolvedDependenciess(st)
	if err != nil {
		return nil, err
	}

	if len(unresolved.deps) == 0 {
		return st, nil
	}

	depMan := NewChartDependencyManager(filename, st.logger)

	return resolveDependencies(st, depMan, unresolved)
}

func resolveDependencies(st *HelmState, depMan *chartDependencyManager, unresolved *UnresolvedDependencies) (*HelmState, error) {
	resolved, lockfileExists, err := depMan.Resolve(unresolved)
	if err != nil {
		return nil, fmt.Errorf("unable to resolve %d deps: %v", len(unresolved.deps), err)
	}
	if !lockfileExists {
		return st, nil
	}

	repoToURL := map[string]string{}

	for _, r := range st.Repositories {
		repoToURL[r.Name] = r.URL
	}

	updated := *st
	for i, r := range updated.Releases {
		repo, chart, ok := resolveRemoteChart(r.Chart)
		if !ok {
			continue
		}

		_, ok = repoToURL[repo]
		// Skip this chart from dependency management, as there's no matching `repository` in the helmfile state,
		// which may imply that this is a local chart within a directory, like `charts/myapp`
		if !ok {
			continue
		}

		ver, err := resolved.Get(chart)
		if err != nil {
			return nil, err
		}

		updated.Releases[i].Version = ver
	}

	return &updated, nil
}

func (st *HelmState) updateDependenciesInTempDir(shell helmexec.DependencyUpdater, tempDir func(string, string) (string, error)) (*HelmState, error) {
	filename, unresolved, err := getUnresolvedDependenciess(st)
	if err != nil {
		return nil, err
	}

	if len(unresolved.deps) == 0 {
		return st, nil
	}

	d, err := tempDir("", "")
	if err != nil {
		return nil, fmt.Errorf("unable to create dir: %v", err)
	}
	defer os.RemoveAll(d)

	return updateDependencies(st, shell, unresolved, filename, d)
}

func getUnresolvedDependenciess(st *HelmState) (string, *UnresolvedDependencies, error) {
	repoToURL := map[string]string{}

	for _, r := range st.Repositories {
		repoToURL[r.Name] = r.URL
	}

	unresolved := &UnresolvedDependencies{deps: map[string]unresolvedChartDependency{}}
	//if err := unresolved.Add("stable/envoy", "https://kubernetes-charts.storage.googleapis.com", ""); err != nil {
	//	panic(err)
	//}

	for _, r := range st.Releases {
		repo, chart, ok := resolveRemoteChart(r.Chart)
		if !ok {
			continue
		}

		url, ok := repoToURL[repo]
		// Skip this chart from dependency management, as there's no matching `repository` in the helmfile state,
		// which may imply that this is a local chart within a directory, like `charts/myapp`
		if !ok {
			continue
		}

		if err := unresolved.Add(chart, url, r.Version); err != nil {
			return "", nil, err
		}
	}

	filename := filepath.Base(st.FilePath)
	filename = strings.TrimSuffix(filename, ".gotmpl")
	filename = strings.TrimSuffix(filename, ".yaml")
	filename = strings.TrimSuffix(filename, ".yml")

	return filename, unresolved, nil
}

func updateDependencies(st *HelmState, shell helmexec.DependencyUpdater, unresolved *UnresolvedDependencies, filename, wd string) (*HelmState, error) {
	depMan := NewChartDependencyManager(filename, st.logger)

	_, err := depMan.Update(shell, wd, unresolved)
	if err != nil {
		return nil, fmt.Errorf("unable to resolve %d deps: %v", len(unresolved.deps), err)
	}

	return resolveDependencies(st, depMan, unresolved)
}

type chartDependencyManager struct {
	Name string

	logger *zap.SugaredLogger

	readFile  func(string) ([]byte, error)
	writeFile func(string, []byte, os.FileMode) error
}

func NewChartDependencyManager(name string, logger *zap.SugaredLogger) *chartDependencyManager {
	return &chartDependencyManager{
		Name:      name,
		readFile:  ioutil.ReadFile,
		writeFile: ioutil.WriteFile,
		logger:    logger,
	}
}

func (m *chartDependencyManager) lockFileName() string {
	return fmt.Sprintf("%s.lock", m.Name)
}

func (m *chartDependencyManager) Update(shell helmexec.DependencyUpdater, wd string, unresolved *UnresolvedDependencies) (*ResolvedDependencies, error) {
	// Generate `Chart.yaml` of the temporary local chart
	if err := m.writeBytes(filepath.Join(wd, "Chart.yaml"), []byte(fmt.Sprintf("name: %s\n", m.Name))); err != nil {
		return nil, err
	}

	// Generate `requirements.yaml` of the temporary local chart from the helmfile state
	reqsContent, err := yaml.Marshal(unresolved.ToChartRequirements())
	if err != nil {
		return nil, err
	}
	if err := m.writeBytes(filepath.Join(wd, "requirements.yaml"), reqsContent); err != nil {
		return nil, err
	}

	// Generate `requirements.lock` of the temporary local chart by coping `<basename>.lock`
	lockFile := m.lockFileName()

	lockFileContent, err := m.readBytes(lockFile)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	if lockFileContent != nil {
		if err := m.writeBytes(filepath.Join(wd, "requirements.lock"), lockFileContent); err != nil {
			return nil, err
		}
	}

	// Update the lock file by running `helm dependency update`
	if err := shell.UpdateDeps(wd); err != nil {
		return nil, err
	}

	updatedLockFileContent, err := m.readBytes(filepath.Join(wd, "requirements.lock"))
	if err != nil {
		return nil, err
	}

	// Commit the lock file if and only if everything looks ok
	if err := m.writeBytes(lockFile, updatedLockFileContent); err != nil {
		return nil, err
	}

	resolved, _, err := m.Resolve(unresolved)
	return resolved, err
}

func (m *chartDependencyManager) Resolve(unresolved *UnresolvedDependencies) (*ResolvedDependencies, bool, error) {
	updatedLockFileContent, err := m.readBytes(m.lockFileName())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, true, nil
		}
		return nil, false, err
	}

	// Load resolved dependencies into memory
	lockedReqs := &ChartLockedRequirements{}
	if err := yaml.Unmarshal(updatedLockFileContent, lockedReqs); err != nil {
		return nil, false, err
	}

	resolved := &ResolvedDependencies{deps: map[string]ResolvedChartDependency{}}
	for _, d := range lockedReqs.ResolvedDependencies {
		if err := resolved.add(d); err != nil {
			return nil, false, err
		}
	}

	return resolved, true, nil
}

func (m *chartDependencyManager) readBytes(filename string) ([]byte, error) {
	bytes, err := m.readFile(filename)
	if err != nil {
		return nil, err
	}
	m.logger.Debugf("readBytes: read from %s:\n%s", filename, bytes)
	return bytes, nil
}

func (m *chartDependencyManager) writeBytes(filename string, data []byte) error {
	err := m.writeFile(filename, data, 0644)
	if err != nil {
		return err
	}
	m.logger.Debugf("writeBytes: wrote to %s:\n%s", filename, data)
	return nil
}