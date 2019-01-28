package dependencies

import (
	"bytes"
	"fmt"
	"github.com/jfrog/gocmd/utils/cache"
	"github.com/jfrog/gocmd/utils/cmd"
	gofrogio "github.com/jfrog/gofrog/io"
	"github.com/jfrog/jfrog-client-go/artifactory/auth"
	"github.com/jfrog/jfrog-client-go/artifactory/buildinfo"
	"github.com/jfrog/jfrog-client-go/httpclient"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	multifilereader "github.com/jfrog/jfrog-client-go/utils/io"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils/checksum"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/pkg/errors"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	FailedToRetrieve          = "Failed to retrieve"
	FromBothArtifactoryAndVcs = "from both Artifactory and VCS"
)

// Collects the dependencies of the project
func CollectProjectDependencies(targetRepo, rootProjectDir string, cache *cache.DependenciesCache, auth auth.ArtifactoryDetails) (map[string]bool, error) {
	dependenciesMap, err := getDependenciesGraphWithFallback(targetRepo, auth)
	if err != nil {
		return nil, err
	}
	replaceDependencies, err := getReplaceDependencies()
	if err != nil {
		return nil, err
	}

	// Merge replaceDependencies with dependenciesToPublish
	mergeReplaceDependenciesWithGraphDependencies(replaceDependencies, dependenciesMap)
	sumFileContent, sumFileStat, err := cmd.GetSumContentAndRemove(rootProjectDir)
	if err != nil {
		return nil, err
	}
	if len(sumFileContent) > 0 && sumFileStat != nil {
		defer cmd.RestoreSumFile(rootProjectDir, sumFileContent, sumFileStat)
	}
	projectDependencies, err := downloadDependencies(targetRepo, cache, dependenciesMap, auth)
	if err != nil {
		return projectDependencies, err
	}
	return projectDependencies, nil
}

func downloadDependencies(targetRepo string, cache *cache.DependenciesCache, depSlice map[string]bool, auth auth.ArtifactoryDetails) (map[string]bool, error) {
	client, err := httpclient.ClientBuilder().Build()
	if err != nil {
		return nil, err
	}
	cacheDependenciesMap := cache.GetMap()
	dependenciesMap := map[string]bool{}
	for module := range depSlice {
		nameAndVersion := strings.Split(module, "@")
		resp, err := performHeadRequest(auth, client, targetRepo, nameAndVersion[0], nameAndVersion[1])
		if err != nil {
			return dependenciesMap, err
		}

		if resp.StatusCode == 200 {
			cacheDependenciesMap[getDependencyName(nameAndVersion[0])+":"+nameAndVersion[1]] = true
			err = downloadDependency(true, module, targetRepo, auth)
			dependenciesMap[module] = true
		} else if resp.StatusCode == 404 {
			cacheDependenciesMap[getDependencyName(nameAndVersion[0])+":"+nameAndVersion[1]] = false
			err = downloadDependency(false, module, "", nil)
			dependenciesMap[module] = false
		}

		if err != nil {
			return dependenciesMap, err
		}
	}
	return dependenciesMap, nil
}

func performHeadRequest(auth auth.ArtifactoryDetails, client *httpclient.HttpClient, targetRepo, module, version string) (*http.Response, error) {
	url := auth.GetUrl() + "api/go/" + targetRepo + "/" + module + "/@v/" + version + ".mod"
	resp, _, err := client.SendHead(url, auth.CreateHttpClientDetails())
	if err != nil {
		return nil, err
	}
	log.Debug("Artifactory head request response for", url, ":", resp.StatusCode)
	return resp, nil
}

// Creating dependency with the mod file in the temp directory
func createDependencyInTemp(zipPath string) (tempDir string, err error) {
	tempDir, err = fileutils.GetTempDirPath()
	if err != nil {
		return "", err
	}
	multiReader, err := multifilereader.NewMultiFileReaderAt([]string{zipPath})
	if err != nil {
		return "", errorutils.CheckError(err)
	}
	err = fileutils.Unzip(multiReader, multiReader.Size(), tempDir)
	if err != nil {
		return "", errorutils.CheckError(err)
	}
	return tempDir, nil
}

func replaceExclamationMarkWithUpperCase(moduleName string) string {
	var str string
	for i := 0; i < len(moduleName); i++ {
		if string(moduleName[i]) == "!" {
			if i < len(moduleName)-1 {
				r := rune(moduleName[i+1])
				str += string(unicode.ToUpper(r))
				i++
			}
		} else {
			str += string(moduleName[i])
		}
	}
	return str
}

// Runs the go mod download command. Should set first the environment variable of GoProxy
func downloadDependency(downloadFromArtifactory bool, fullDependencyName, targetRepo string, auth auth.ArtifactoryDetails) error {
	var err error
	if downloadFromArtifactory {
		log.Debug("Downloading dependency from Artifactory:", fullDependencyName)
		err = cmd.SetGoProxyEnvVar(auth.GetUrl(), auth.GetUser(), auth.GetPassword(), targetRepo)
	} else {
		log.Debug("Downloading dependency from VCS:", fullDependencyName)
		err = os.Unsetenv(cmd.GOPROXY)
	}
	if errorutils.CheckError(err) != nil {
		return err
	}

	err = cmd.DownloadDependency(fullDependencyName)
	return err
}

// Downloads the mod file from Artifactory to the Go cache
func downloadModFileFromArtifactoryToLocalCache(cachePath, targetRepo, name, version string, auth auth.ArtifactoryDetails, client *httpclient.HttpClient) string {
	pathToModuleCache := filepath.Join(cachePath, name, "@v")
	dirExists, err := fileutils.IsDirExists(pathToModuleCache, false)
	if err != nil {
		log.Error(fmt.Sprintf("Received an error: %s for %s@%s", err, name, version))
		return ""
	}

	if dirExists {
		url := auth.GetUrl() + "api/go/" + targetRepo + "/" + name + "/@v/" + version + ".mod"
		log.Debug("Downloading mod file from Artifactory:", url)
		downloadFileDetails := &httpclient.DownloadFileDetails{
			FileName: version + ".mod",
			// Artifactory URL
			DownloadPath:  url,
			LocalPath:     pathToModuleCache,
			LocalFileName: version + ".mod",
		}
		resp, err := client.DownloadFile(downloadFileDetails, "", auth.CreateHttpClientDetails(), 3, false)
		if err != nil {
			log.Error(fmt.Sprintf("Received an error %s downloading a file: %s to the local path: %s", err.Error(), downloadFileDetails.FileName, downloadFileDetails.LocalPath))
			return ""
		}

		log.Debug(fmt.Sprintf("Received %d from Artifactory %s", resp.StatusCode, url))
		return filepath.Join(downloadFileDetails.LocalPath, downloadFileDetails.LocalFileName)
	}
	return ""
}

func GetRegex() (regExp *RegExp, err error) {
	emptyRegex, err := cmd.GetRegExp(`^\s*require (?:[\(\w\.@:%_\+-.~#?&]?.+)`)
	if err != nil {
		return
	}

	indirectRegex, err := cmd.GetRegExp(`(// indirect)$`)
	if err != nil {
		return
	}

	generatedBy, err := cmd.GetRegExp(`^(// )`)
	if err != nil {
		return
	}

	regExp = &RegExp{
		notEmptyModRegex: emptyRegex,
		indirectRegex:    indirectRegex,
		generatedBy:      generatedBy,
	}
	return
}

func downloadAndCreateDependency(cachePath, name, version, fullDependencyName, targetRepo string, downloadedFromArtifactory bool, auth auth.ArtifactoryDetails) (*Package, error) {
	// Dependency is missing within the cache. Need to download it...
	err := downloadDependency(downloadedFromArtifactory, fullDependencyName, targetRepo, auth)
	if err != nil {
		return nil, err
	}
	// Now that this dependency in the cache, get the dependency object
	dep, err := createDependency(cachePath, name, version)
	if err != nil {
		return nil, err
	}
	return dep, nil
}

func logError(err error) {
	if err != nil {
		log.Error("Received an error:", err)
	}
}

func shouldDownloadFromArtifactory(module, version, targetRepo string, auth auth.ArtifactoryDetails, client *httpclient.HttpClient) (bool, error) {
	res, err := performHeadRequest(auth, client, targetRepo, module, version)
	if err != nil {
		return false, err
	}
	if res.StatusCode == 200 {
		return true, nil
	}
	return false, nil
}

func GetDependencies(cachePath string, moduleSlice map[string]bool) ([]Package, error) {
	var deps []Package
	for module := range moduleSlice {
		moduleInfo := strings.Split(module, "@")
		name := getDependencyName(moduleInfo[0])
		dep, err := createDependency(cachePath, name, moduleInfo[1])
		if err != nil {
			return nil, err
		}
		if dep != nil {
			deps = append(deps, *dep)
		}
	}
	return deps, nil
}

// Returns the actual path to the dependency.
// If in the path there are capital letters, the Go convention is to use "!" before the letter.
// The letter itself in lowercase.
func getDependencyName(name string) string {
	path := ""
	for _, letter := range name {
		if unicode.IsUpper(letter) {
			path += "!" + strings.ToLower(string(letter))
		} else {
			path += string(letter)
		}
	}
	return path
}

// Creates a go dependency.
// Returns a nil value in case the dependency does not include a zip in the cache.
func createDependency(cachePath, dependencyName, version string) (*Package, error) {
	// We first check if the this dependency has a zip binary in the local go cache.
	// If it does not, nil is returned. This seems to be a bug in go.
	zipPath, err := getPackageZipLocation(cachePath, dependencyName, version)

	if err != nil {
		return nil, err
	}

	if zipPath == "" {
		return nil, nil
	}

	dep := Package{}

	dep.id = strings.Join([]string{dependencyName, version}, ":")
	dep.version = version
	dep.zipPath = zipPath
	dep.modPath = filepath.Join(cachePath, dependencyName, "@v", version+".mod")
	dep.modContent, err = ioutil.ReadFile(dep.modPath)
	if err != nil {
		return &dep, errorutils.CheckError(err)
	}

	// Mod file dependency for the build-info
	modDependency := buildinfo.Dependency{Id: dep.id}
	checksums, err := checksum.Calc(bytes.NewBuffer(dep.modContent))
	if err != nil {
		return &dep, err
	}
	modDependency.Checksum = &buildinfo.Checksum{Sha1: checksums[checksum.SHA1], Md5: checksums[checksum.MD5]}

	// Zip file dependency for the build-info
	zipDependency := buildinfo.Dependency{Id: dep.id}
	fileDetails, err := fileutils.GetFileDetails(dep.zipPath)
	if err != nil {
		return &dep, err
	}
	zipDependency.Checksum = &buildinfo.Checksum{Sha1: fileDetails.Checksum.Sha1, Md5: fileDetails.Checksum.Md5}

	dep.buildInfoDependencies = append(dep.buildInfoDependencies, modDependency, zipDependency)
	return &dep, nil
}

// Returns the path to the package zip file if exists.
func getPackageZipLocation(cachePath, dependencyName, version string) (string, error) {
	zipPath, err := getPackagePathIfExists(cachePath, dependencyName, version)
	if err != nil {
		return "", err
	}

	if zipPath != "" {
		return zipPath, nil
	}

	zipPath, err = getPackagePathIfExists(filepath.Dir(cachePath), dependencyName, version)

	if err != nil {
		return "", err
	}

	return zipPath, nil
}

// Validates if the package zip file exists.
func getPackagePathIfExists(cachePath, dependencyName, version string) (zipPath string, err error) {
	zipPath = filepath.Join(cachePath, dependencyName, "@v", version+".zip")
	fileExists, err := fileutils.IsFileExists(zipPath, false)
	if err != nil {
		log.Warn(fmt.Sprintf("Could not find zip binary for dependency '%s' at %s.", dependencyName, zipPath))
		return "", err
	}
	// Zip binary does not exist, so we skip it by returning a nil dependency.
	if !fileExists {
		log.Debug("The following file is missing:", zipPath)
		return "", nil
	}
	return zipPath, nil
}

func getGOPATH() (string, error) {
	goCmd, err := cmd.NewCmd()
	if err != nil {
		return "", err
	}
	goCmd.Command = []string{"env", "GOPATH"}
	output, err := gofrogio.RunCmdOutput(goCmd)
	if errorutils.CheckError(err) != nil {
		return "", fmt.Errorf("Could not find GOPATH env: %s", err.Error())
	}
	return strings.TrimSpace(string(output)), nil
}

func mergeReplaceDependenciesWithGraphDependencies(replaceDeps []string, graphDeps map[string]bool) {
	for _, replaceLine := range replaceDeps {
		// Remove unnecessary spaces
		replaceLine = strings.TrimSpace(replaceLine)
		log.Debug("Working on the following replace line:", replaceLine)
		// Split to get the right side that is the replace of the dependency
		replaceDeps := strings.Split(replaceLine, "=>")
		// Perform validation
		if len(replaceDeps) < 2 {
			log.Debug("The following replace line includes less then two elements", replaceDeps)
			continue
		}
		replacesInfo := strings.TrimSpace(replaceDeps[1])
		newDependency := strings.Split(replacesInfo, " ")
		if len(newDependency) != 2 {
			log.Debug("The replacer is not pointing to a VCS version", newDependency[0])
			continue
		}
		// Check if the dependency in the map, if not add to the map
		_, exists := graphDeps[newDependency[0]+"@"+newDependency[1]]
		if !exists {
			log.Debug("Adding dependency", newDependency[0], newDependency[1])
			graphDeps[newDependency[0]+"@"+newDependency[1]] = true
		}
	}
}

func getReplaceDependencies() ([]string, error) {
	replaceRegExp, err := cmd.GetRegExp(`\s*replace (?:[\(\w\.@:%_\+-.~#?&]?.+)`)
	if err != nil {
		return nil, err
	}
	rootDir, err := cmd.GetProjectRoot()
	if err != nil {
		return nil, err
	}
	modFilePath := filepath.Join(rootDir, "go.mod")
	modFileContent, err := ioutil.ReadFile(modFilePath)
	if err != nil {
		return nil, err
	}
	replaceDependencies := replaceRegExp.FindAllString(string(modFileContent), -1)
	return replaceDependencies, nil
}

// Runs go mod graph command with fallback.
func getDependenciesGraphWithFallback(targetRepo string, auth auth.ArtifactoryDetails) (map[string]bool, error) {
	dependenciesMap := map[string]bool{}
	modulesWithErrors := map[string]previousTries{}
	usedProxy := true
	for true {
		// Configuring each run to use Artifactory/VCS
		err := setOrUnsetGoProxy(usedProxy, targetRepo, auth)
		if err != nil {
			return nil, err
		}
		usedProxy = !usedProxy
		dependenciesMap, err = cmd.GetDependenciesGraph()
		if err == nil {
			break
		}
		moduleAndVersion, err := getModuleAndVersion(usedProxy, err)
		if err != nil {
			return nil, err
		}
		modulePreviousTries, ok := modulesWithErrors[moduleAndVersion]
		modulePreviousTries.setTriedFrom(usedProxy)
		if ok && modulePreviousTries.triedFromVCS && modulePreviousTries.triedFromArtifactory {
			return nil, errorutils.CheckError(errors.New(fmt.Sprintf(FailedToRetrieve+" %s "+FromBothArtifactoryAndVcs, moduleAndVersion)))
		}
		modulesWithErrors[moduleAndVersion] = modulePreviousTries
	}
	return dependenciesMap, nil
}

func setOrUnsetGoProxy(usedProxy bool, targetRepo string, auth auth.ArtifactoryDetails) error {
	if !usedProxy {
		log.Debug("Trying download the dependencies from Artifactory...")
		return cmd.SetGoProxyEnvVar(auth.GetUrl(), auth.GetUser(), auth.GetPassword(), targetRepo)
	} else {
		log.Debug("Trying download the dependencies from the VCS...")
		return errorutils.CheckError(os.Unsetenv(cmd.GOPROXY))
	}
}

func getModuleAndVersion(usedProxy bool, err error) (string, error) {
	splittedLine := strings.Split(err.Error(), ":")
	logDebug(err, usedProxy)
	if len(splittedLine) < 2 {
		return "", errorutils.CheckError(errors.New("Missing module name and version in the error message " + err.Error()))
	}
	return strings.TrimSpace(splittedLine[1]), nil
}

func logDebug(err error, usedProxy bool) {
	message := "Received " + err.Error() + " from"
	if usedProxy {
		message += " Artifactory."
	} else {
		message += " VCS."
	}
	log.Debug(message)
}

func populateModWithTidy(path string) error {
	err := os.Chdir(filepath.Dir(path))
	if errorutils.CheckError(err) != nil {
		return err
	}
	log.Debug("Preparing to populate mod", filepath.Dir(path))
	err = removeGoSum(path)
	logError(err)
	// Running go mod tidy command
	err = cmd.RunGoModTidy()
	if err != nil {
		return err
	}

	return nil
}

func removeGoSum(path string) error {
	// Remove go.sum file to avoid checksum conflicts with the old go.sum
	goSum := filepath.Join(filepath.Dir(path), "go.sum")
	exists, err := fileutils.IsFileExists(goSum, false)
	if err != nil {
		return err
	}
	if exists {
		err = os.Remove(goSum)
		if errorutils.CheckError(err) != nil {
			return err
		}
	}
	return nil
}

func runGoModGraph() (output map[string]bool, err error) {
	// Running go mod graph command
	return cmd.GetDependenciesGraph()
}

type previousTries struct {
	triedFromArtifactory bool
	triedFromVCS         bool
}

func (pt *previousTries) setTriedFrom(usedProxy bool) {
	if usedProxy {
		pt.triedFromArtifactory = true
	} else {
		pt.triedFromVCS = true
	}
}