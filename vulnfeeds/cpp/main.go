package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/exp/slices"

	"github.com/google/osv/vulnfeeds/cves"
	"github.com/google/osv/vulnfeeds/git"
	"github.com/google/osv/vulnfeeds/utility"
	"github.com/google/osv/vulnfeeds/vulns"
)

type VendorProduct struct {
	Vendor  string
	Product string
}

func (vp *VendorProduct) UnmarshalText(text []byte) error {
	s := strings.Split(string(text), ":")
	vp.Vendor = s[0]
	vp.Product = s[1]
	return nil
}

type VendorProductToRepoMap map[VendorProduct][]string

type CVEIDString string

type ConversionOutcome int

var ErrNoRanges = errors.New("no ranges")

var ErrUnresolvedFix = errors.New("fixes not resolved to commits")

func (c ConversionOutcome) String() string {
	return [...]string{"ConversionUnknown", "Successful", "Rejected", "NoSoftware", "NoRepos", "NoRanges", "FixUnresolvable"}[c]
}

const (
	extension = ".json"
)

const (
	// Set of enums for categorizing conversion outcomes.
	ConversionUnknown ConversionOutcome = iota // Shouldn't happen
	Successful                                 // It worked!
	Rejected                                   // The CVE was rejected
	NoSoftware                                 // The CVE had no CPEs relating to software (i.e. Operating Systems or Hardware).
	NoRepos                                    // The CPE Vendor/Product had no repositories derived for it.
	NoRanges                                   // No viable commit ranges could be calculated from the repository for the CVE's CPE(s).
	FixUnresolvable                            // Partial resolution of versions, resulting in a false positive.
)

var (
	jsonPath            = flag.String("nvd_json", "", "Path to NVD CVE JSON to examine.")
	parsedCPEDictionary = flag.String("cpe_repos", "", "Path to JSON mapping of CPEs to repos generated by cperepos")
	outDir              = flag.String("out_dir", "", "Path to output results.")
	outFormat           = flag.String("out_format", "OSV", "Format to output {OSV,PackageInfo}")
)
var Logger utility.LoggerWrapper
var RepoTagsCache git.RepoTagsCache
var Metrics struct {
	TotalCVEs           int
	CVEsForApplications int
	CVEsForKnownRepos   int
	OSVRecordsGenerated int
	Outcomes            map[CVEIDString]ConversionOutcome // Per-CVE-ID record of conversion result.
}

// References with these tags have been found to contain completely unrelated
// repositories and can be misleading as to the software's true repository,
// Currently not used for this purpose due to undesired false positives
// reducing the number of valid records successfully converted.
var RefTagDenyList = []string{
	// "Exploit",
	// "Third Party Advisory",
	"Broken Link", // Actively ignore these though.
}

// VendorProducts known not to be Open Source software and causing
// cross-contamination of repo derivation between CVEs.
var VendorProductDenyList = []VendorProduct{
	// Causes a chain reaction of incorrect associations from CVE-2022-2068
	// {"netapp", "ontap_select_deploy_administration_utility"},
	// Causes misattribution for Python, e.g. CVE-2022-26488
	// {"netapp", "active_iq_unified_manager"},
	// Causes misattribution for OpenSSH, e.g. CVE-2021-28375
	// {"netapp", "cloud_backup"},
	// Three strikes and the entire netapp vendor is out...
	{"netapp", ""},
	// [CVE-2021-28957]: Incorrectly associates with github.com/lxml/lxml
	{"oracle", "zfs_storage_appliance_kit"},
	{"gradle", "enterprise"}, // The OSS repo gets mis-attributed via CVE-2020-15767
}

// Looks at what the repo to determine if it contains code using an in-scope language
func InScopeRepo(repoURL string) bool {
	parsedURL, err := url.Parse(repoURL)
	if err != nil {
		Logger.Infof("Warning: %s failed to parse, skipping", repoURL)
		return false
	}

	switch parsedURL.Hostname() {
	case "github.com":
		return InScopeGitHubRepo(repoURL)
	default:
		return InScopeGitRepo(repoURL)
	}
}

// Use the GitHub API to query the repository's language metadata to make the determination.
func InScopeGitHubRepo(repoURL string) bool {
	// TODO(apollock): Implement
	return true
}

// Clone the repo and look for C/C++ files to make the determination.
func InScopeGitRepo(repoURL string) bool {
	// TODO(apollock): Implement
	return true
}

// Examines repos and tries to convert versions to commits by treating them as Git tags.
// Takes a CVE ID string (for logging), cves.VersionInfo with AffectedVersions and
// typically no AffectedCommits and attempts to add AffectedCommits (including Fixed commits) where there aren't any.
func GitVersionsToCommits(CVE string, versions cves.VersionInfo, repos []string, cache git.RepoTagsCache) (v cves.VersionInfo, e error) {
	// versions is a VersionInfo with AffectedVersions and typically no AffectedCommits
	// v is a VersionInfo with AffectedCommits (containing Fixed commits) included
	v = versions
	for _, repo := range repos {
		normalizedTags, err := git.NormalizeRepoTags(repo, cache)
		if err != nil {
			Logger.Warnf("[%s]: Failed to normalize tags for %s: %v", CVE, repo, err)
			continue
		}
		for _, av := range versions.AffectedVersions {
			if av.Introduced != "" {
				ac, err := git.VersionToCommit(av.Introduced, repo, cves.Introduced, normalizedTags)
				if err != nil {
					Logger.Warnf("[%s]: Failed to get a Git commit for introduced version %q from %q: %v", CVE, av.Introduced, repo, err)
				} else {
					Logger.Infof("[%s]: Successfully derived %+v for introduced version %q", CVE, ac, av.Introduced)
					v.AffectedCommits = append(v.AffectedCommits, ac)
				}
			}
			// Only try and convert fixed versions to commits via tags if there aren't any Fixed commits already.
			// cves.ExtractVersionInfo() opportunistically returns
			// AffectedCommits (with Fixed commits) when the CVE has appropriate references.
			if v.HasFixedCommits(repo) && av.Fixed != "" {
				Logger.Infof("[%s]: Using preassumed fixed commits %+v instead of deriving from fixed version %q", CVE, v.FixedCommits(repo), av.Fixed)
			} else if av.Fixed != "" {
				ac, err := git.VersionToCommit(av.Fixed, repo, cves.Fixed, normalizedTags)
				if err != nil {
					Logger.Warnf("[%s]: Failed to get a Git commit for fixed version %q from %q: %v", CVE, av.Fixed, repo, err)
				} else {
					Logger.Infof("[%s]: Successfully derived %+v for fixed version %q", CVE, ac, av.Fixed)
					v.AffectedCommits = append(v.AffectedCommits, ac)
				}
			}
			// Only try and convert last_affected versions to commits via tags if there aren't any Fixed commits already (to maintain schema compliance).
			// cves.ExtractVersionInfo() opportunistically returns
			// AffectedCommits (with Fixed commits) when the CVE has appropriate references.
			if !v.HasFixedCommits(repo) && av.LastAffected != "" {
				ac, err := git.VersionToCommit(av.LastAffected, repo, cves.LastAffected, normalizedTags)
				if err != nil {
					Logger.Warnf("[%s]: Failed to get a Git commit for last_affected version %q from %q: %v", CVE, av.LastAffected, repo, err)
				} else {
					Logger.Infof("[%s]: Successfully derived %+v for last_affected version %q", CVE, ac, av.LastAffected)
					v.AffectedCommits = append(v.AffectedCommits, ac)
				}
			}
		}
	}
	return v, nil
}

func refAcceptable(ref cves.CVEReferenceData, tagDenyList []string) bool {
	for _, deniedTag := range tagDenyList {
		if slices.Contains(ref.Tags, deniedTag) {
			return false
		}
	}
	return true
}

// Examines the CVE references for a CVE and derives repos for it, optionally caching it.
func ReposFromReferences(CVE string, cache VendorProductToRepoMap, vp *VendorProduct, refs []cves.CVEReferenceData, tagDenyList []string) (repos []string) {
	// This currently only gets called for cache misses, but make it not rely on that assumption.
	if vp != nil {
		if cachedRepos, ok := cache[*vp]; ok {
			return cachedRepos
		}
	}
	for _, ref := range refs {
		// If any of the denylist tags are in the ref's tag set, it's out of consideration.
		if !refAcceptable(ref, tagDenyList) {
			// Also remove it if previously added under an acceptable tag.
			maybeRemoveFromVPRepoCache(cache, vp, ref.URL)
			Logger.Infof("[%s]: disregarding %q for %q due to a denied tag in %q", CVE, ref.URL, vp, ref.Tags)
			break
		}
		repo, err := cves.Repo(ref.URL)
		if err != nil {
			// Failed to parse as a valid repo.
			continue
		}
		if !git.ValidRepo(repo) {
			continue
		}
		if slices.Contains(repos, repo) {
			continue
		}
		repos = append(repos, repo)
		maybeUpdateVPRepoCache(cache, vp, repo)
	}
	return repos
}

// Takes an NVD CVE record and outputs an OSV file in the specified directory.
func CVEToOSV(CVE cves.CVEItem, repos []string, cache git.RepoTagsCache, directory string) error {
	CVEID := CVE.CVE.CVEDataMeta.ID // For brevity.
	CPEs := cves.CPEs(CVE)
	// The vendor name and product name are used to construct the output `vulnDir` below, so need to be set to *something* to keep the output tidy.
	maybeVendorName := "ENOCPE"
	maybeProductName := "ENOCPE"

	if len(CPEs) > 0 {
		CPE, err := cves.ParseCPE(CPEs[0]) // For naming the subdirectory used for output.
		maybeVendorName = CPE.Vendor
		maybeProductName = CPE.Product
		if err != nil {
			return fmt.Errorf("[%s]: Can't generate an OSV record without valid CPE data", CVEID)
		}
	}

	v, notes := vulns.FromCVE(CVEID, CVE)
	versions, versionNotes := cves.ExtractVersionInfo(CVE, nil)
	notes = append(notes, versionNotes...)

	if len(versions.AffectedVersions) != 0 {
		var err error
		// There are some AffectedVersions to try and resolve to AffectedCommits.
		if len(repos) == 0 {
			return fmt.Errorf("[%s]: No affected ranges for %q, and no repos to try and convert %+v to tags with", CVEID, maybeProductName, versions.AffectedVersions)
		}
		Logger.Infof("[%s]: Trying to convert version tags %+v to commits using %v", CVEID, versions.AffectedVersions, repos)
		versions, err = GitVersionsToCommits(CVEID, versions, repos, cache)
		if err != nil {
			return fmt.Errorf("[%s]: Failed to convert version tags to commits: %#v", CVEID, err)
		}
		hasAnyFixedCommits := false
		for _, repo := range repos {
			if versions.HasFixedCommits(repo) {
				hasAnyFixedCommits = true
			}
		}

		if versions.HasFixedVersions() && !hasAnyFixedCommits {
			return fmt.Errorf("[%s]: Failed to convert fixed version tags to commits: %#v %w", CVEID, versions, ErrUnresolvedFix)
		}
	}

	affected := vulns.Affected{}
	affected.AttachExtractedVersionInfo(versions)
	v.Affected = append(v.Affected, affected)

	if len(v.Affected[0].Ranges) == 0 {
		return fmt.Errorf("[%s]: No affected ranges detected for %q %w", CVEID, maybeProductName, ErrNoRanges)
	}

	vulnDir := filepath.Join(directory, maybeVendorName, maybeProductName)
	err := os.MkdirAll(vulnDir, 0755)
	if err != nil {
		Logger.Warnf("Failed to create dir: %v", err)
		return fmt.Errorf("failed to create dir: %v", err)
	}
	outputFile := filepath.Join(vulnDir, v.ID+extension)
	notesFile := filepath.Join(vulnDir, v.ID+".notes")
	f, err := os.Create(outputFile)
	if err != nil {
		Logger.Warnf("Failed to open %s for writing: %v", outputFile, err)
		return fmt.Errorf("failed to open %s for writing: %v", outputFile, err)
	}
	defer f.Close()
	err = v.ToJSON(f)
	if err != nil {
		Logger.Warnf("Failed to write %s: %v", outputFile, err)
		return fmt.Errorf("failed to write %s: %v", outputFile, err)
	}
	Logger.Infof("[%s]: Generated OSV record for %q", CVEID, maybeProductName)
	if len(notes) > 0 {
		err = os.WriteFile(notesFile, []byte(strings.Join(notes, "\n")), 0660)
		if err != nil {
			Logger.Warnf("[%s]: Failed to write %s: %v", CVEID, notesFile, err)
		}
	}
	return nil
}

// Takes an NVD CVE record and outputs a PackageInfo struct in a file in the specified directory.
func CVEToPackageInfo(CVE cves.CVEItem, repos []string, cache git.RepoTagsCache, directory string) error {
	CVEID := CVE.CVE.CVEDataMeta.ID // For brevity.
	CPEs := cves.CPEs(CVE)
	// The vendor name and product name are used to construct the output `vulnDir` below, so need to be set to *something* to keep the output tidy.
	maybeVendorName := "ENOCPE"
	maybeProductName := "ENOCPE"

	if len(CPEs) > 0 {
		CPE, err := cves.ParseCPE(CPEs[0]) // For naming the subdirectory used for output.
		maybeVendorName = CPE.Vendor
		maybeProductName = CPE.Product
		if err != nil {
			return fmt.Errorf("[%s]: Can't generate an OSV record without valid CPE data", CVEID)
		}
	}

	// more often than not, this yields a VersionInfo with AffectedVersions and no AffectedCommits.
	versions, notes := cves.ExtractVersionInfo(CVE, nil)

	if len(versions.AffectedVersions) != 0 {
		var err error
		// There are some AffectedVersions to try and resolve to AffectedCommits.
		if len(repos) == 0 {
			return fmt.Errorf("[%s]: No affected ranges for %q, and no repos to try and convert %+v to tags with", CVEID, maybeProductName, versions.AffectedVersions)
		}
		Logger.Infof("[%s]: Trying to convert version tags %+v to commits using %v", CVEID, versions.AffectedVersions, repos)
		versions, err = GitVersionsToCommits(CVEID, versions, repos, cache)
		if err != nil {
			return fmt.Errorf("[%s]: Failed to convert version tags to commits: %#v", CVEID, err)
		}
	}

	hasAnyFixedCommits := false
	for _, repo := range repos {
		if versions.HasFixedCommits(repo) {
			hasAnyFixedCommits = true
		}
	}

	if versions.HasFixedVersions() && !hasAnyFixedCommits {
		return fmt.Errorf("[%s]: Failed to convert fixed version tags to commits: %#v %w", CVEID, versions, ErrUnresolvedFix)
	}

	if len(versions.AffectedCommits) == 0 {
		return fmt.Errorf("[%s]: No affected commit ranges determined for %q %w", CVEID, maybeProductName, ErrNoRanges)
	}

	versions.AffectedVersions = nil // these have served their purpose and are not required in the resulting output.

	var pkgInfos []vulns.PackageInfo
	pi := vulns.PackageInfo{VersionInfo: versions}
	pkgInfos = append(pkgInfos, pi) // combine-to-osv expects a serialised *array* of PackageInfo

	vulnDir := filepath.Join(directory, maybeVendorName, maybeProductName)
	err := os.MkdirAll(vulnDir, 0755)
	if err != nil {
		Logger.Warnf("Failed to create dir: %v", err)
		return fmt.Errorf("failed to create dir: %v", err)
	}

	outputFile := filepath.Join(vulnDir, CVEID+".nvd"+extension)
	notesFile := filepath.Join(vulnDir, CVEID+".nvd.notes")
	f, err := os.Create(outputFile)
	if err != nil {
		Logger.Warnf("Failed to open %s for writing: %v", outputFile, err)
		return fmt.Errorf("failed to open %s for writing: %v", outputFile, err)
	}
	defer f.Close()

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(&pkgInfos)

	if err != nil {
		Logger.Warnf("Failed to encode PackageInfo to %s: %v", outputFile, err)
		return fmt.Errorf("failed to encode PackageInfo to %s: %v", outputFile, err)
	}

	Logger.Infof("[%s]: Generated PackageInfo record for %q", CVEID, maybeProductName)

	if len(notes) > 0 {
		err = os.WriteFile(notesFile, []byte(strings.Join(notes, "\n")), 0660)
		if err != nil {
			Logger.Warnf("[%s]: Failed to write %s: %v", CVEID, notesFile, err)
		}
	}

	return nil
}

func loadCPEDictionary(ProductToRepo *VendorProductToRepoMap, f string) error {
	data, err := os.ReadFile(f)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &ProductToRepo)
}

// Adds the repo to the cache for the Vendor/Product combination if not already present.
func maybeUpdateVPRepoCache(cache VendorProductToRepoMap, vp *VendorProduct, repo string) {
	if vp == nil {
		return
	}
	if slices.Contains(cache[*vp], repo) {
		return
	}
	cache[*vp] = append(cache[*vp], repo)
}

// Removes the repo from the cache for the Vendor/Product combination if already present.
func maybeRemoveFromVPRepoCache(cache VendorProductToRepoMap, vp *VendorProduct, repo string) {
	if vp == nil {
		return
	}
	cacheEntry, ok := cache[*vp]
	if !ok {
		return
	}
	if !slices.Contains(cacheEntry, repo) {
		return
	}
	i := slices.Index(cacheEntry, repo)
	if i == -1 {
		return
	}
	// If there is only one entry, delete the entry cache entry.
	if len(cacheEntry) == 1 {
		delete(cache, *vp)
		return
	}
	cacheEntry = slices.Delete(cacheEntry, i, i+1)
	cache[*vp] = cacheEntry
}

// Output a CSV summarizing per-CVE how it was handled.
func outputOutcomes(outcomes map[CVEIDString]ConversionOutcome, reposForCVE map[CVEIDString][]string, directory string) error {
	outcomesFile, err := os.Create(filepath.Join(directory, "outcomes.csv"))
	if err != nil {
		return err
	}
	defer outcomesFile.Close()
	w := csv.NewWriter(outcomesFile)
	w.Write([]string{"CVE", "outcome", "repos"})
	for CVE, outcome := range outcomes {
		// It's conceivable to have more than one repo for a CVE, so concatenate them.
		r := ""
		if repos, ok := reposForCVE[CVE]; ok {
			r = strings.Join(repos, " ")
		}
		w.Write([]string{string(CVE), outcome.String(), r})
	}
	w.Flush()

	if err = w.Error(); err != nil {
		return err
	}
	return nil
}

func main() {
	flag.Parse()
	if !slices.Contains([]string{"OSV", "PackageInfo"}, *outFormat) {
		fmt.Fprintf(os.Stderr, "Unsupported output format: %s\n", *outFormat)
		os.Exit(1)
	}

	Metrics.Outcomes = make(map[CVEIDString]ConversionOutcome)

	var logCleanup func()
	Logger, logCleanup = utility.CreateLoggerWrapper("cpp-osv")
	defer logCleanup()

	data, err := os.ReadFile(*jsonPath)
	if err != nil {
		Logger.Fatalf("Failed to open file: %v", err) // double check this is best practice output
	}

	var parsed cves.NVDCVE
	err = json.Unmarshal(data, &parsed)
	if err != nil {
		Logger.Fatalf("Failed to parse NVD CVE JSON: %v", err)
	}

	VPRepoCache := make(VendorProductToRepoMap)

	if *parsedCPEDictionary != "" {
		err = loadCPEDictionary(&VPRepoCache, *parsedCPEDictionary)
		if err != nil {
			Logger.Fatalf("Failed to load parsed CPE dictionary: %v", err)
		}
		Logger.Infof("VendorProductToRepoMap cache has %d entries preloaded", len(VPRepoCache))
	}

	ReposForCVE := make(map[CVEIDString][]string)

	for _, cve := range parsed.CVEItems {
		refs := cve.CVE.References.ReferenceData
		CPEs := cves.CPEs(cve)
		CVEID := CVEIDString(cve.CVE.CVEDataMeta.ID)

		if len(refs) == 0 && len(CPEs) == 0 {
			Logger.Infof("[%s]: skipping due to lack of CPEs and lack of references", CVEID)
			// 100% of these in 2022 were rejected CVEs
			Metrics.Outcomes[CVEID] = Rejected
			continue
		}

		// Edge case: No CPEs, but perhaps usable references.
		if len(refs) > 0 && len(CPEs) == 0 {
			repos := ReposFromReferences(string(CVEID), nil, nil, refs, RefTagDenyList)
			if len(repos) == 0 {
				Logger.Warnf("[%s]: Failed to derive any repos and there were no CPEs", CVEID)
				continue
			}
			Logger.Infof("[%s]: Derived %q for CVE with no CPEs", CVEID, repos)
			ReposForCVE[CVEID] = repos
		}

		// Does it have any application CPEs? Look for pre-computed repos based on VendorProduct.
		appCPECount := 0
		for _, CPEstr := range cves.CPEs(cve) {
			CPE, err := cves.ParseCPE(CPEstr)
			if err != nil {
				Logger.Warnf("[%s]: Failed to parse CPE %q: %+v", cve.CVE.CVEDataMeta.ID, CPEstr, err)
				Metrics.Outcomes[CVEID] = ConversionUnknown
				continue
			}
			if CPE.Part == "a" {
				appCPECount += 1
			}
			if _, ok := VPRepoCache[VendorProduct{CPE.Vendor, CPE.Product}]; ok {
				Logger.Infof("[%s]: Pre-references, derived %q for %q %q using cache", CVEID, VPRepoCache[VendorProduct{CPE.Vendor, CPE.Product}], CPE.Vendor, CPE.Product)
				if _, ok := ReposForCVE[CVEID]; !ok {
					ReposForCVE[CVEID] = VPRepoCache[VendorProduct{CPE.Vendor, CPE.Product}]
					continue
				}
				// Don't append duplicates.
				for _, repo := range VPRepoCache[VendorProduct{CPE.Vendor, CPE.Product}] {
					if !slices.Contains(ReposForCVE[CVEID], repo) {
						ReposForCVE[CVEID] = append(ReposForCVE[CVEID], repo)
					}
				}
			}
		}

		if len(CPEs) > 0 && appCPECount == 0 {
			// This CVE is not for software (based on there being CPEs but not any application ones), skip.
			Metrics.Outcomes[CVEID] = NoSoftware
			continue
		}

		if appCPECount > 0 {
			Metrics.CVEsForApplications++
		}

		// If there wasn't a repo from the CPE Dictionary, try and derive one from the CVE references.
		if _, ok := ReposForCVE[CVEID]; !ok && len(refs) > 0 {
			for _, CPEstr := range cves.CPEs(cve) {
				CPE, err := cves.ParseCPE(CPEstr)
				if err != nil {
					Logger.Warnf("[%s]: Failed to parse CPE %q: %+v", CVEID, CPEstr, err)
					continue
				}
				// Continue to only focus on application CPEs.
				if CPE.Part != "a" {
					continue
				}
				if slices.Contains(VendorProductDenyList, VendorProduct{CPE.Vendor, ""}) {
					continue
				}
				if slices.Contains(VendorProductDenyList, VendorProduct{CPE.Vendor, CPE.Product}) {
					continue
				}
				repos := ReposFromReferences(string(CVEID), VPRepoCache, &VendorProduct{CPE.Vendor, CPE.Product}, refs, RefTagDenyList)
				if len(repos) == 0 {
					Logger.Warnf("[%s]: Failed to derive any repos for %q %q", CVEID, CPE.Vendor, CPE.Product)
					continue
				}
				Logger.Infof("[%s]: Derived %q for %q %q", CVEID, repos, CPE.Vendor, CPE.Product)
				ReposForCVE[CVEID] = repos
			}
		}

		Logger.Infof("[%s]: Summary: [CPEs=%d AppCPEs=%d DerivedRepos=%d]", CVEID, len(CPEs), appCPECount, len(ReposForCVE[CVEID]))

		// If we've made it to here, we may have a CVE:
		// * that has Application-related CPEs (so applies to software)
		// * has a reference that is a known repository URL
		// OR
		// * a derived repository for the software package
		//
		// We do not yet have:
		// * any knowledge of the language used
		// * definitive version information

		if _, ok := ReposForCVE[CVEID]; !ok {
			// We have nothing useful to work with, so we'll assume it's out of scope
			Logger.Infof("[%s]: Passing due to lack of viable repository", CVEID)
			Metrics.Outcomes[CVEID] = NoRepos
			continue
		}

		Logger.Infof("[%s]: Repos: %#v", CVEID, ReposForCVE[CVEID])

		for _, repo := range ReposForCVE[CVEID] {
			if !InScopeRepo(repo) {
				continue
			}
		}

		Metrics.CVEsForKnownRepos++

		switch *outFormat {
		case "OSV":
			err = CVEToOSV(cve, ReposForCVE[CVEID], RepoTagsCache, *outDir)
		case "PackageInfo":
			err = CVEToPackageInfo(cve, ReposForCVE[CVEID], RepoTagsCache, *outDir)
		}
		// Parse this error to determine which failure mode it was
		if err != nil {
			Logger.Warnf("[%s]: Failed to generate an OSV record: %+v", CVEID, err)
			if errors.Is(err, ErrNoRanges) {
				Metrics.Outcomes[CVEID] = NoRanges
				continue
			}
			if errors.Is(err, ErrUnresolvedFix) {
				Metrics.Outcomes[CVEID] = FixUnresolvable
				continue
			}
			Metrics.Outcomes[CVEID] = ConversionUnknown
			continue
		}
		Metrics.OSVRecordsGenerated++
		Metrics.Outcomes[CVEID] = Successful
	}
	Metrics.TotalCVEs = len(parsed.CVEItems)
	err = outputOutcomes(Metrics.Outcomes, ReposForCVE, *outDir)
	if err != nil {
		// Log entry with size 1.15M exceeds maximum size of 256.0K
		fmt.Fprintf(os.Stderr, "Failed to write out metrics: %v", err)
	}
	// Outcomes is too big to log, so zero it out.
	Metrics.Outcomes = nil
	Logger.Infof("%s Metrics: %+v", filepath.Base(*jsonPath), Metrics)
}
