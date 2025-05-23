package tengo

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// This file contains public functions and structs designed to make integration
// testing easier. These functions are used in Tengo's own tests, but may also
// be useful to other packages and applications using Tengo as a library.

// IntegrationTestSuite is the interface for a suite of test methods. In
// addition to implementing the 3 methods of the interface, an integration test
// suite struct should have any number of test methods of form
// TestFoo(t *testing.T), which will be executed automatically by RunSuite.
type IntegrationTestSuite interface {
	Setup(backend string) error
	Teardown(backend string) error
	BeforeTest(backend string) error
}

// RunSuite runs all test methods in the supplied suite once per backend. It
// calls suite.Setup(backend) once per backend, then iterates through all Test
// methods in suite. For each test method, suite.BeforeTest will be run,
// followed by the test itself. Finally, suite.Teardown(backend) will be run.
// Backends are just strings, and may contain docker image names or any other
// string representation that the test suite understands.
func RunSuite(suite IntegrationTestSuite, t *testing.T, backends []string) {
	var suiteName string
	suiteType := reflect.TypeOf(suite)
	suiteVal := reflect.ValueOf(suite)
	if suiteVal.Kind() == reflect.Ptr {
		suiteName = suiteVal.Elem().Type().Name()
	} else {
		suiteName = suiteType.Name()
	}

	if len(backends) == 0 {
		t.Skipf("Skipping integration test suite %s: No backends supplied", suiteName)
	}

	for _, backend := range backends {
		if err := suite.Setup(backend); err != nil {
			t.Fatalf("RunSuite %s: Setup(%s) failed: %s", suiteName, backend, err)
		}

		// Run test methods
		for n := 0; n < suiteType.NumMethod(); n++ {
			method := suiteType.Method(n)
			if strings.HasPrefix(method.Name, "Test") {
				subtestName := fmt.Sprintf("%s.%s:%s", suiteName, method.Name, backend)
				subtest := func(subt *testing.T) {
					if err := suite.BeforeTest(backend); err != nil {
						suite.Teardown(backend)
						t.Fatalf("RunSuite %s: BeforeTest(%s) failed: %s", suiteName, backend, err)
					}
					// Capture output and only display if test fails or is skipped. Note that
					// this approach does not permit concurrent subtest execution.
					realOut, realErr := os.Stdout, os.Stderr
					realLogOutput := log.StandardLogger().Out
					if r, w, err := os.Pipe(); err == nil {
						os.Stdout = w
						os.Stderr = w
						log.SetOutput(w)
						outChan := make(chan []byte)
						defer func() {
							iface := recover()
							w.Close()
							os.Stdout = realOut
							os.Stderr = realErr
							log.SetOutput(realLogOutput)
							testOutput := <-outChan
							if subt.Failed() || subt.Skipped() || iface != nil {
								os.Stderr.Write(testOutput)
							}
							if iface != nil {
								os.Stderr.WriteString(fmt.Sprintf("panic: %v [recovered]\n\n", iface))
								os.Stderr.Write(debug.Stack())
								subt.Fail()
							}
						}()
						go func() {
							var b bytes.Buffer
							_, err := io.Copy(&b, r) // prevent pipe from filling up
							if err == nil {
								outChan <- b.Bytes()
							} else {
								outChan <- fmt.Appendf(nil, "Unable to buffer test output: %v", err)
							}
							close(outChan)
						}()
					}
					method.Func.Call([]reflect.Value{reflect.ValueOf(suite), reflect.ValueOf(subt)})
				}
				t.Run(subtestName, subtest)
			}
		}

		if err := suite.Teardown(backend); err != nil {
			t.Fatalf("RunSuite %s: Teardown(%s) failed: %s", suiteName, backend, err)
		}
	}
}

// SkeemaTestImages examines the SKEEMA_TEST_IMAGES env variable (which
// should be set to a comma-separated list of Docker images) and returns a slice
// of strings. It may perform some conversions in the process, if the configured
// images are only available from non-Dockerhub locations. If no images are
// configured, the test will be marked as skipped. If any configured images are
// known to be unavailable for the system's architecture, the test is marked as
// failed.
func SkeemaTestImages(t *testing.T) []string {
	t.Helper()
	envString := strings.TrimSpace(os.Getenv("SKEEMA_TEST_IMAGES"))
	if envString == "" {
		fmt.Println("*** IMPORTANT ***")
		fmt.Println("SKEEMA_TEST_IMAGES env var is not set, so integration tests will be skipped.")
		fmt.Println("The VAST majority of Skeema's test coverage is in these integration tests!")
		fmt.Println("To run integration tests, you may set SKEEMA_TEST_IMAGES to a comma-separated")
		fmt.Println("list of Docker images. For example:")
		fmt.Println(`$ SKEEMA_TEST_IMAGES="mysql:8.0,mariadb:10.11" go test`)
		t.SkipNow()
	}

	arch, err := DockerEngineArchitecture()
	if err != nil {
		t.Fatalf("Unable to obtain Docker engine architecture: %v", err)
	}

	images := strings.Split(envString, ",")
	for n, image := range images {
		if arch == "arm64" && (strings.HasPrefix(image, "percona:5") || strings.HasPrefix(image, "mysql:5")) {
			// No MySQL 5.x or Percona Server 5.x builds available for arm64
			t.Fatalf("SKEEMA_TEST_IMAGES env var includes %s, but this image is not available for %s", image, arch)
		} else if strings.HasPrefix(image, "percona:8") {
			// Top-level (Docker Inc maintained) images for Percona Server 8.0 appear to
			// not be updated frequently anymore and lack arm builds, so always use
			// percona/percona-server instead
			images[n] = strings.Replace(image, "percona:", "percona/percona-server:", 1)
		}
	}
	return images
}

// SkeemaTestContainerCleanup potentially performs cleanup on a container used
// in integration testing, depending on the value of the SKEEMA_TEST_CLEANUP
// env variable:
//   - If SKEEMA_TEST_CLEANUP is set to "stop" (case-insensitive), the supplied
//     container will be stopped.
//   - If SKEEMA_TEST_CLEANUP is set to "none" (case-insensitive), no cleanup
//     action is taken, and the supplied container remains running.
//   - Otherwise (if SKEEMA_TEST_CLEANUP is not set, or set to any other value),
//     the supplied container will be removed (destroyed) if it has a name
//     beginning with "skeema-test-". With any other name prefix, an error is
//     returned.
func SkeemaTestContainerCleanup(d *DockerizedInstance) error {
	action := strings.TrimSpace(os.Getenv("SKEEMA_TEST_CLEANUP"))
	if strings.EqualFold(action, "none") {
		return nil
	} else if strings.EqualFold(action, "stop") {
		return d.Stop()
	} else if strings.HasPrefix(d.containerName, "skeema-test-") {
		return d.Destroy()
	} else {
		return fmt.Errorf("refusing to destroy container %q without skeema-test- naming prefix", d.containerName)
	}
}

// EnableTLS copies the contents of certsDir to the provided container, adds
// server configuration to use those certs, and then restarts the database.
func EnableTLS(dinst *DockerizedInstance, certsDir string) error {
	if err := dinst.PutFile(certsDir, "/tls"); err != nil {
		return err
	}

	commands := []string{
		"chown root:root /tls/tls.cnf",
		"mv /tls/tls.cnf /etc/mysql/conf.d/ || mv /tls/tls.cnf /etc/my.cnf.d/", // most images use /etc/mysql/conf.d but others do not
		"chown -R mysql:root /tls",
		"chmod o-r /tls/*.pem",
	}
	for _, command := range commands {
		toRun := []string{"/bin/sh", "-c", command}
		_, errStr, err := dinst.Exec(toRun, nil)
		if err != nil {
			return fmt.Errorf("%w: %s", err, errStr)
		}
	}

	if err := dinst.Stop(); err != nil {
		return err
	}
	if err := dinst.Start(); err != nil {
		return err
	}
	return dinst.TryConnect()
}
