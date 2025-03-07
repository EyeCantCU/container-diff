/*
Copyright 2018 Google, Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package differs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"

	pkgutil "github.com/EyeCantCU/container-diff/pkg/util"
	"github.com/EyeCantCU/container-diff/util"
	godocker "github.com/fsouza/go-dockerclient"

	"github.com/nightlyone/lockfile"
	"github.com/sirupsen/logrus"
)

// RPM macros file location
const rpmMacros string = "/usr/lib/rpm/macros"

// RPM command to extract packages from the rpm database
var rpmCmd = []string{
	"rpm", "--nodigest", "--nosignature",
	"-qa", "--qf", "%{NAME}\t%{VERSION}-%{RELEASE}\t%{SIZE}\n",
}
var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

// daemonMutex is required to protect against other go-routines, as
// nightlyone/lockfile implements a recursive lock, which doesn't protect
// against other go-routines that have the same PID.  Note that the mutex
// *must* always be locked prior to the lockfile, and unlocked after.
var daemonMutex sync.Mutex

type RPMAnalyzer struct {
}

// Name returns the name of the analyzer.
func (a RPMAnalyzer) Name() string {
	return "RPMAnalyzer"
}

// Diff compares the installed rpm packages of image1 and image2.
func (a RPMAnalyzer) Diff(image1, image2 pkgutil.Image) (util.Result, error) {
	diff, err := singleVersionDiff(image1, image2, a)
	return diff, err
}

// Analyze collects information of the installed rpm packages on image.
func (a RPMAnalyzer) Analyze(image pkgutil.Image) (util.Result, error) {
	analysis, err := singleVersionAnalysis(image, a)
	return analysis, err
}

// getPackages returns a map of installed rpm package on image.
func (a RPMAnalyzer) getPackages(image pkgutil.Image) (map[string]util.PackageInfo, error) {
	path := image.FSPath
	packages := make(map[string]util.PackageInfo)
	if _, err := os.Stat(path); err != nil {
		// invalid image directory path
		return packages, err
	}

	// try to find the rpm binary in bin/ or usr/bin/
	rpmBinary := filepath.Join(path, "bin/rpm")
	if _, err := os.Stat(rpmBinary); err != nil {
		rpmBinary = filepath.Join(path, "usr/bin/rpm")
		if _, err = os.Stat(rpmBinary); err != nil {
			logrus.Errorf("Could not detect RPM binary in unpacked image %s", image.Source)
			return packages, nil
		}
	}

	packages, err := rpmDataFromImageFS(image)
	if err != nil {
		logrus.Info("Couldn't retrieve RPM data from extracted filesystem; running query in container")
		return rpmDataFromContainer(image.Image)
	}
	return packages, err
}

// rpmDataFromImageFS runs a local rpm binary, if any, to query the image
// rpmdb and returns a map of installed packages.
func rpmDataFromImageFS(image pkgutil.Image) (map[string]util.PackageInfo, error) {
	dbPath, err := rpmEnvCheck(image.FSPath)
	if err != nil {
		logrus.Warnf("Couldn't find RPM database: %s", err.Error())
		return nil, err
	}
	return rpmDataFromFS(image.FSPath, dbPath)
}

// rpmEnvCheck checks there is an rpm binary in the host and tries to
// get the RPM database path from the /usr/lib/rpm/macros file in the
// image rootfs
func rpmEnvCheck(rootFSPath string) (string, error) {
	if err := exec.Command("rpm", "--version").Run(); err != nil {
		logrus.Warn("No RPM binary in host")
		return "", err
	}
	imgMacrosFile, err := os.Open(filepath.Join(rootFSPath, rpmMacros))
	if err != nil {
		return "", err
	}
	defer imgMacrosFile.Close()

	scanner := bufio.NewScanner(imgMacrosFile)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// We are looking for a macro definition like (form openSUSE Leap):
		// %_dbpath                %{_usr}/lib/sysimage/rpm
		if strings.HasPrefix(line, "%_dbpath") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				break
			}
			out, err := exec.Command("rpm", "-E", fields[1]).Output()
			if err != nil {
				return "", err
			}
			dbPath := strings.TrimRight(string(out), "\r\n")
			_, err = os.Stat(filepath.Join(rootFSPath, dbPath))
			return dbPath, err
		}
	}
	return "", errors.New("Failed parsing macros file")
}

// rpmDataFromContainer runs image in a container, queries the data of
// installed rpm packages and returns a map of packages.
func rpmDataFromContainer(image v1.Image) (map[string]util.PackageInfo, error) {
	packages := make(map[string]util.PackageInfo)

	client, err := godocker.NewClientFromEnv()
	if err != nil {
		return packages, err
	}
	if err := lock(); err != nil {
		return packages, err
	}

	imageName, err := loadImageToDaemon(image)

	if err != nil {
		return packages, fmt.Errorf("Error loading image: %s", err)
	}
	unlock()

	defer client.RemoveImage(imageName)
	defer logrus.Infof("Removing image %s", imageName)

	contConf := godocker.Config{
		Entrypoint: rpmCmd,
		Image:      imageName,
	}

	hostConf := godocker.HostConfig{
		AutoRemove: true,
	}

	contOpts := godocker.CreateContainerOptions{Config: &contConf}
	container, err := client.CreateContainer(contOpts)
	if err != nil {
		return packages, err
	}
	logrus.Infof("Created container %s", container.ID)

	removeOpts := godocker.RemoveContainerOptions{
		ID: container.ID,
	}
	defer client.RemoveContainer(removeOpts)

	if err := client.StartContainer(container.ID, &hostConf); err != nil {
		return packages, err
	}

	exitCode, err := client.WaitContainer(container.ID)
	if err != nil {
		return packages, err
	}

	outBuf := new(bytes.Buffer)
	errBuf := new(bytes.Buffer)
	logOpts := godocker.LogsOptions{
		Context:      context.Background(),
		Container:    container.ID,
		Stdout:       true,
		Stderr:       true,
		OutputStream: outBuf,
		ErrorStream:  errBuf,
	}

	if err := client.Logs(logOpts); err != nil {
		return packages, err
	}

	if exitCode != 0 {
		return packages, fmt.Errorf("non-zero exit code %d: %s", exitCode, errBuf.String())
	}

	output := strings.Split(outBuf.String(), "\n")
	return parsePackageData(output)
}

// parsePackageData parses the package data of each line in rpmOutput and
// returns a map of packages.
func parsePackageData(rpmOutput []string) (map[string]util.PackageInfo, error) {
	packages := make(map[string]util.PackageInfo)

	for _, output := range rpmOutput {
		spl := strings.Split(output, "\t")
		if len(spl) != 3 {
			// ignore the empty (last) line
			if output != "" {
				logrus.Errorf("unexpected rpm-query output: '%s'", output)
			}
			continue
		}
		pkg := util.PackageInfo{}

		var err error
		pkg.Size, err = strconv.ParseInt(spl[2], 10, 64)
		if err != nil {
			return packages, fmt.Errorf("error converting package size: %s", spl[2])
		}

		pkg.Version = spl[1]
		packages[spl[0]] = pkg
	}

	return packages, nil
}

// loadImageToDaemon loads the image specified to the docker daemon.
func loadImageToDaemon(img v1.Image) (string, error) {
	tag := generateValidImageTag()
	resp, err := daemon.Write(tag, img)
	if err != nil {
		return "", err
	}
	logrus.Infof("daemon response: %s", resp)
	return tag.Name(), nil
}

// generate random image name until we find one that isn't in use
func generateValidImageTag() name.Tag {
	var tag name.Tag
	var err error
	var i int
	b := make([]rune, 12)
	for {
		for i = 0; i < len(b); i++ {
			b[i] = letters[rand.Intn(len(letters))]
		}
		tag, err = name.NewTag("rpm_test_image:"+string(b), name.WeakValidation)
		if err != nil {
			logrus.Warn(err.Error())
			continue
		}
		img, _ := daemon.Image(tag)
		if img == nil {
			break
		}
	}
	return tag
}

// unlock returns the containerdiff file-system lock.  It is placed in the
// system's temporary directory to make sure it's accessible for all users in
// the system; no root required.
func getLockfile() (lockfile.Lockfile, error) {
	lockPath := filepath.Join(os.TempDir(), ".containerdiff.lock")
	lock, err := lockfile.New(lockPath)
	if err != nil {
		return lock, err
	}
	return lock, nil
}

// lock acquires the containerdiff file-system lock.
func lock() error {
	var err error
	var lock lockfile.Lockfile

	daemonMutex.Lock()
	lock, err = getLockfile()
	if err != nil {
		daemonMutex.Unlock()
		return fmt.Errorf("[lock] cannot init lockfile: %v", err)
	}

	// Try to acquire the lock and in case of a temporary error, sleep for
	// two seconds until the next retry (at most 10 times).  Return fatal
	// errors immediately, as we can't recover.
	for i := 0; i < 10; i++ {
		if err = lock.TryLock(); err != nil {
			switch err.(type) {
			case lockfile.TemporaryError:
				logrus.Debugf("[lock] busy: next retry in two seconds")
				time.Sleep(2 * time.Second)
			default:
				daemonMutex.Unlock()
				return fmt.Errorf("[lock] error acquiring lock: %s", err)
			}
		}
	}
	if err != nil {
		daemonMutex.Unlock()
		return fmt.Errorf("[lock] error acquiring lock: too many tries")
	}

	logrus.Debugf("[lock] lock acquired")
	return nil
}

// unlock releases the containerdiff file-system lock.  Note that errors can be
// ignored as there's no meaningful way to recover.
func unlock() error {
	lock, err := getLockfile()
	if err != nil {
		return fmt.Errorf("[unlock] cannot init lockfile: %v", err)
	}
	err = lock.Unlock()
	if err != nil {
		return fmt.Errorf("[unlock] error releasing lock: %s", err)
	}
	logrus.Debugf("[unlock] lock released")
	daemonMutex.Unlock()
	return nil
}

type RPMLayerAnalyzer struct {
}

// Name returns the name of the analyzer.
func (a RPMLayerAnalyzer) Name() string {
	return "RPMLayerAnalyzer"
}

// Diff compares the installed rpm packages of image1 and image2 for each layer
func (a RPMLayerAnalyzer) Diff(image1, image2 pkgutil.Image) (util.Result, error) {
	diff, err := singleVersionLayerDiff(image1, image2, a)
	return diff, err
}

// Analyze collects information of the installed rpm packages on each layer
func (a RPMLayerAnalyzer) Analyze(image pkgutil.Image) (util.Result, error) {
	analysis, err := singleVersionLayerAnalysis(image, a)
	return analysis, err
}

// getPackages returns an array of maps of installed rpm packages on each layer
func (a RPMLayerAnalyzer) getPackages(image pkgutil.Image) ([]map[string]util.PackageInfo, error) {
	path := image.FSPath
	var packages []map[string]util.PackageInfo
	if _, err := os.Stat(path); err != nil {
		// invalid image directory path
		return packages, err
	}

	// try to find the rpm binary in bin/ or usr/bin/
	rpmBinary := filepath.Join(path, "bin/rpm")
	if _, err := os.Stat(rpmBinary); err != nil {
		rpmBinary = filepath.Join(path, "usr/bin/rpm")
		if _, err = os.Stat(rpmBinary); err != nil {
			logrus.Errorf("Could not detect RPM binary in unpacked image %s", image.Source)
			return packages, nil
		}
	}

	packages, err := rpmDataFromLayerFS(image)
	if err != nil {
		logrus.Info("Couldn't retrieve RPM data from extracted filesystem; running query in container")
		return rpmDataFromLayeredContainers(image.Image)
	}
	return packages, err
}

// rpmDataFromLayerFS runs a local rpm binary, if any, to query the layer
// rpmdb and returns an array of maps of installed packages.
func rpmDataFromLayerFS(image pkgutil.Image) ([]map[string]util.PackageInfo, error) {
	var packages []map[string]util.PackageInfo
	dbPath, err := rpmEnvCheck(image.FSPath)
	if err != nil {
		logrus.Warnf("Couldn't find RPM database: %s", err.Error())
		return packages, err
	}
	for _, layer := range image.Layers {
		layerPackages, err := rpmDataFromFS(layer.FSPath, dbPath)
		if err != nil {
			return packages, err
		}
		packages = append(packages, layerPackages)
	}

	return packages, nil
}

// rpmDataFromFS runs a local rpm binary to query the image
// rpmdb and returns a map of installed packages.
func rpmDataFromFS(fsPath string, dbPath string) (map[string]util.PackageInfo, error) {
	packages := make(map[string]util.PackageInfo)
	if _, err := os.Stat(filepath.Join(fsPath, dbPath)); err == nil {
		cmdArgs := append([]string{"--root", fsPath, "--dbpath", dbPath}, rpmCmd[1:]...)
		out, err := exec.Command(rpmCmd[0], cmdArgs...).Output()
		if err != nil {
			logrus.Warnf("RPM call failed: %s", err.Error())
			return packages, err
		}
		output := strings.Split(string(out), "\n")
		packages, err = parsePackageData(output)
		if err != nil {
			return packages, err
		}
	}
	return packages, nil
}

// rpmDataFromLayeredContainers runs a tmp image in a container for each layer,
// queries the data of installed rpm packages and returns an array of maps of
// packages.
func rpmDataFromLayeredContainers(image v1.Image) ([]map[string]util.PackageInfo, error) {
	var packages []map[string]util.PackageInfo
	tmpImage, err := random.Image(0, 0)
	if err != nil {
		return packages, err
	}
	layers, err := image.Layers()
	if err != nil {
		return packages, err
	}
	// Append layers one by one to an empty image and query rpm
	// database on each iteration
	for _, layer := range layers {
		tmpImage, err = mutate.AppendLayers(tmpImage, layer)
		if err != nil {
			return packages, err
		}
		layerPackages, err := rpmDataFromContainer(tmpImage)
		if err != nil {
			return packages, err
		}
		packages = append(packages, layerPackages)
	}

	return packages, nil
}
