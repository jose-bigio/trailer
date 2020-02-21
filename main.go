package main

import (
	"encoding/csv"
	"encoding/gob"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v2"

	"github.com/educlos/testrail"
	"github.com/urfave/cli"

	"github.com/docker/trailer/spec"
)

type Suite struct {
	ProjectID   int            `yaml:"project_id"`
	SuiteID     int            `yaml:"suite_id"`
	LastUpdated string         `yaml:"last_updated"`
	Cases       map[int]string `yaml:"cases"`
}

func main() {
	var (
		verbose   bool
		dry       bool
		retries   int
		runID     int
		suiteID   int
		projectID int
		comment   string
		file      string
	)

	app := cli.NewApp()
	app.HideHelp = true
	app.HideVersion = true
	app.Name = "trailer"
	app.Usage = "TestRail command line utility"
	app.Commands = []cli.Command{
		{
			Name:    "upload",
			Aliases: []string{"u"},
			Usage:   "Upload JUnit XML reports to TestRail",
			Flags: []cli.Flag{
				// TODO: Respect verbosity and use a proper logging library
				cli.BoolFlag{
					Name:        "verbose, v",
					Usage:       "turn on debug logs",
					Destination: &verbose,
				},
				cli.BoolFlag{
					Name:        "dry, d",
					Usage:       "print readable results without updating TestRail run",
					Destination: &dry,
				},
				cli.IntFlag{
					Name:        "ignore-failures, i",
					Usage:       "ignore failures and retry this number of times",
					Destination: &retries,
					Value:       1,
				},
				cli.IntFlag{
					Name:        "run-id, r",
					Usage:       "TestRail run ID to target for the update",
					Destination: &runID,
				},
				cli.StringFlag{
					Name:        "comment, c",
					Usage:       "prefix to use when commenting on TestRail updates",
					Destination: &comment,
				},
			},
			ArgsUsage: "[input *.xml files...]",
			Action: func(c *cli.Context) error {
				username := os.Getenv("TESTRAIL_USERNAME")
				token := os.Getenv("TESTRAIL_TOKEN")

				if username == "" || token == "" {
					log.Fatalf("Need to set TESTRAIL_USERNAME and TESTRAIL_TOKEN")
				}

				if runID == 0 {
					log.Fatalf("Must set --run-id to a non-zero integer")
				}

				updates := spec.Updates{
					ResultMap: map[int]spec.Update{},
				}

				suites := spec.JUnitTestSuites{}
				for _, file := range c.Args() {
					newSuites, err := spec.ParseFile(file)
					if err != nil {
						log.Fatalf(fmt.Sprintf("Failed to parse file: %s", err))
					}

					suites.Suites = append(suites.Suites, newSuites...)
				}

				updates.AddSuites(comment, suites)

				if !dry {
					client := testrail.NewClient("https://docker.testrail.com", username, token)
					for i := 0; i < retries; i++ {
						results, err := updates.CreatePayload()
						if err != nil {
							log.Fatalf(fmt.Sprintf("Failed to create results payload: %s", err))
						}
						results, err = pruneResults(client, runID, results)
						if err != nil {
							fmt.Printf("%v", err)
							log.Fatalf("Failed to prune Test Results")
						}
						r, err := client.AddResultsForCases(runID, results)
						if err != nil {
							errString := err.Error()
							if strings.Contains(errString, "400 Bad Request") {
								regex, err := regexp.Compile("case C([\\d]+) unknown")
								if err != nil {
									log.Fatalf("failed to compile test case regex: %s", err)
								}
								ids := regex.FindAllStringSubmatch(errString, -1)
								for _, id := range ids {
									if len(id) != 2 {
										log.Fatalf("failed to parse case ID")
									}
									caseID, err := strconv.Atoi(id[1])
									if err != nil {
										log.Fatalf("failed to convert case ID to integer: %s", err)
									}
									updates.RemoveResult(caseID)
								}
							} else {
								log.Fatalf(fmt.Sprintf("Failed to upload test results to TestRail: %s", err))
							}
						}

						if len(r) == 0 {
							log.Print("No results uploaded")
						} else {
							for _, res := range r {
								fmt.Printf("%+v\n", res)
							}
							break
						}
					}
				}

				return nil
			},
		},
		{
			Name:    "download",
			Aliases: []string{"d"},
			Usage:   "Download case specs from TestRail",
			Flags: []cli.Flag{
				cli.BoolFlag{
					Name:        "verbose, v",
					Usage:       "turn on debug logs",
					Destination: &verbose,
				},
				cli.IntFlag{
					Name:        "project-id, p",
					Usage:       "TestRail project ID to download cases from",
					Destination: &projectID,
				},
				cli.IntFlag{
					Name:        "suite-id, s",
					Usage:       "TestRail suite ID to download cases from",
					Destination: &suiteID,
				},
				cli.StringFlag{
					Name:        "file, f",
					Usage:       "File to write downloaded cases to",
					Destination: &file,
				},
			},
			Action: func(c *cli.Context) error {
				username := os.Getenv("TESTRAIL_USERNAME")
				token := os.Getenv("TESTRAIL_TOKEN")

				if username == "" || token == "" {
					log.Fatalf("Need to set TESTRAIL_USERNAME and TESTRAIL_TOKEN")
				}

				if projectID == 0 {
					log.Fatalf("Must set --project-id to a non-zero integer")
				}

				if suiteID == 0 {
					log.Fatalf("Must set --suite-id to a non-zero integer")
				}

				client := testrail.NewClient("https://docker.testrail.com", username, token)
				cases, err := client.GetCases(projectID, suiteID)
				if err != nil {
					log.Fatalf("Error getting cases: %s", err)
				}

				s := Suite{
					LastUpdated: time.Unix(0, 0).Format(time.RFC3339Nano),
					ProjectID:   projectID,
					SuiteID:     suiteID,
					Cases:       map[int]string{},
				}

				if file != "" {
					if _, err = os.Stat(file); err == nil {
						data, err := ioutil.ReadFile(file)
						if err != nil {
							log.Fatalf("Error reading file: %s", err)
						}

						err = yaml.Unmarshal(data, &s)
						if err != nil {
							log.Fatalf("Error unmarshaling suite data: %s", err)
						}
					}
				}

				lastUpdated, err := time.Parse(time.RFC3339Nano, s.LastUpdated)
				if err != nil {
					log.Fatalf("Error parsing last_updated time: %s", err)
				}

				updated := false
				for _, c := range cases {
					if lastUpdated.Before(time.Unix(int64(c.UdpatedOn), 0)) {
						s.Cases[c.ID] = c.Title
						updated = true
					}
				}

				if updated {
					s.LastUpdated = time.Now().Format(time.RFC3339Nano)
					data, err := yaml.Marshal(&s)
					if err != nil {
						log.Fatalf("Error marshaling suite data: %s", err)
					}

					if file != "" {
						err = ioutil.WriteFile(file, data, 0644)
						if err != nil {
							log.Fatalf("Error writing suite data to output file: %s", err)
						}
					} else {
						log.Print(string(data))
					}
				}

				return nil
			},
		},
		{
			Name:    "prune",
			Aliases: []string{"p"},
			Usage:   "Prune case specs from a cases file",
			Flags: []cli.Flag{
				cli.BoolFlag{
					Name:        "verbose, v",
					Usage:       "turn on debug logs",
					Destination: &verbose,
				},
				cli.StringFlag{
					Name:        "file, f",
					Usage:       "File to write downloaded cases to",
					Destination: &file,
				},
			},
			ArgsUsage: "[input case IDs...]",
			Action: func(c *cli.Context) error {
				if file == "" {
					log.Fatal("Must specify an input cases file")
				}

				s := Suite{
					LastUpdated: time.Unix(0, 0).Format(time.RFC3339Nano),
					ProjectID:   projectID,
					SuiteID:     suiteID,
					Cases:       map[int]string{},
				}

				data, err := ioutil.ReadFile(file)
				if err != nil {
					log.Fatalf("Error reading file: %s", err)
				}

				err = yaml.Unmarshal(data, &s)
				if err != nil {
					log.Fatalf("Error unmarshaling suite data: %s", err)
				}

				caseIDsToPrune := []int{}
				for _, iString := range c.Args() {
					i, err := strconv.Atoi(iString)
					if err != nil {
						log.Fatalf("Cannot convert string to int: %s", err)
					}
					caseIDsToPrune = append(caseIDsToPrune, i)
				}

				updated := false
				for _, i := range caseIDsToPrune {
					if _, ok := s.Cases[i]; ok {
						delete(s.Cases, i)
						updated = true
					}
				}

				if updated {
					s.LastUpdated = time.Now().Format(time.RFC3339Nano)
					data, err := yaml.Marshal(&s)
					if err != nil {
						log.Fatalf("Error marshaling suite data: %s", err)
					}

					if file != "" {
						err = ioutil.WriteFile(file, data, 0644)
						if err != nil {
							log.Fatalf("Error writing suite data to output file: %s", err)
						}
					} else {
						log.Print(string(data))
					}
				}

				return nil
			},
		},
		{
			Name:  "migrate",
			Usage: "migrate from docker testrail account to Mirantis testrail account",
			Flags: []cli.Flag{
				cli.BoolFlag{
					Name:        "verbose, v",
					Usage:       "turn on debug logs",
					Destination: &verbose,
				},
			},
			ArgsUsage: "[input csv directory]",
			Action: func(c *cli.Context) error {
				// DOCKER
				// TODO: could make a more user friendly interface for discovering
				// projectId and suiteIds

				// obtained by using client.GetProject(False)
				// then printing the project.Name, and project.ID to find the associated
				// id for CAAS
				// to get suite ID use client.GetSuites(projectID) and print out suite.Name, suite.ID
				var dockerProjectID int = 3
				// suite 33 is for the DTR project
				var dockerSuiteID int = 33
				// Docker suite Ids

				var mirantisProjectID int = 21
				var mirantisSuiteID int = 10657

				caseIDString := "Case ID"
				commentString := "Comment"
				statusString := "Status"
				runString := "Run"

				// validate the input arg
				if len(c.Args()) != 1 {
					log.Fatal("Incorrect usage: trailer migrate <directory>")
				}

				dockerClient, err := createClient("", "https://docker.testrail.com")
				if err != nil {
					log.Fatal(err)
				}

				mirantisClient, err := createClient("MIRANTIS_", "https://mirantis.testrail.com")
				if err != nil {
					log.Fatal(err)
				}

				// Mirantis Suite IDs
				// DTR 10657
				// DTR 2.4.4 10658
				// DTR 2.5.5 10659
				// DTR Archive 10660

				// from http://docs.gurock.com/testrail-api2/reference-statuses
				// the statuses greater than 6 are custom statuses which are not aligned between Docker and Mirantis
				// below is the output from getting the status info GetStatuses() and printing Name, Label, ID
				//
				// passed Passed 1
				// blocked Blocked 2
				// untested Untested 3
				// retest Retest 4
				// failed Failed 5
				// custom_status1 WontTest 6
				// custom_status2 MixedSuccess 7
				// custom_status3 WontFix 8
				// custom_status4 InProgress 9
				// not_relevant NotRelevant 10
				//
				//  Mirantis statuses
				// passed Passed 1
				// product_failed ProdFailed 8
				// test_failed TestFailed 9
				// wont_fix WontFix 10
				// skipped Skipped 6
				// retest Retest 4
				// failed Failed 5
				// blocked Blocked 2
				// untested Untested 3
				// in_progress InProgress 7
				// mixes_success MixedSuccess 11
				// wont_test WontTest 12

				statusCodes := map[string]int{"Passed": 1, "Blocked": 2, "Untested": 3, "Retest": 4, "Failed": 5, "WontTest": 12, "MixedSuccess": 11, "WontFix": 10, "InProgress": 7, "NotRelevant": 12}
				// creates a map of Docker case ids to Mirantis case Ids
				dToMCaseIds := createMaps(dockerClient, mirantisClient, dockerProjectID, dockerSuiteID, mirantisProjectID, mirantisSuiteID)

				// these were the missing test cases
				// should probably figure out how these were dropped,
				// but for the sake of time mapping them here manually
				dToMCaseIds[61947] = 4875610
				dToMCaseIds[61948] = 4875611
				dToMCaseIds[61949] = 4875612
				dToMCaseIds[61950] = 4875613

				filepath.Walk(c.Args()[0], func(path string, info os.FileInfo, err error) error {
					if err != nil {
						fmt.Printf("unable to access %q: %v\n", path, err)
						return err
					}

					if info.IsDir() {
						return nil
					}

					// if the file contains "csv" then parse the file
					// create a corresponding run in the Docker test rail account
					// map the Docker case IDs to the Mirantis case IDs and create
					//
					if strings.HasSuffix(path, "csv") {
						csvFile, err := os.Open(path)
						if err != nil {
							log.Fatal(err)
						}
						defer csvFile.Close()
						csvReader := csv.NewReader(csvFile)
						csvReader.LazyQuotes = true

						// read the first row and create a map
						// from header to column number
						row, err := csvReader.Read()
						if err != nil {
							log.Fatal(err)
						}

						var headers map[string]int = make(map[string]int)
						for i, element := range row {
							// enforce no duplicate headers in the csv
							if _, ok := headers[element]; ok {
								log.Fatalf("%s is a duplicate header", element)
							}
							headers[element] = i
						}

						// set the test run name because it is the same across
						// all columns
						var runName string
						var caseIDs []int
						var results []testrail.ResultsForCase

						// continue reading the remainder of the rows to populate results for
						// new test run
						for {
							row, err := csvReader.Read()
							if err == io.EOF {
								break
							}

							if err != nil {
								log.Fatal(err)
							}

							if runName == "" {
								if i, ok := headers[runString]; ok {
									runName = row[i]
									fmt.Printf("Migrating %s\n", runName)
								} else {
									// this is strict but the name should be populated
									log.Fatalf("%s not found in headers")
								}
							}

							var mCaseID int
							if caseID, ok := headers[caseIDString]; ok {
								caseIDInt, err := strconv.Atoi(row[caseID][1:])
								if err != nil {
									log.Fatal(err)
								}

								// assume that most case Ids will be found, and ignore the ones
								// that are not
								if mCaseID, ok = dToMCaseIds[caseIDInt]; ok {
									caseIDs = append(caseIDs, mCaseID)
								} else {
									// go next row if case id is not found, because
									// result can't be entered
									fmt.Printf("Could not find %d in case ID lookup\n", caseIDInt)
									continue
								}
							} else {
								log.Fatalf("%s not in row", caseIDString)
							}

							var statusInt int
							var i int
							var ok bool

							if i, ok = headers[commentString]; !ok {
								log.Fatalf("Could not find %s in headers", commentString)
							}
							comment := row[i]

							if i, ok = headers[statusString]; !ok {
								log.Fatalf("Could not find %s in headers", statusString)
							}
							status := row[i]

							// the untested status is unsupported
							//  "400 Bad Request", body: {"error":"Field :results.status_id uses an invalid status (Untested)."}
							// so will deal with this by continuing at this point the
							// case is added to the case id but the results are not added
							// basically this means that a result needs to have information and untested is not a result
							if status == "Untested" {
								continue
							}

							if statusInt, ok = statusCodes[status]; !ok {
								log.Fatalf("Could not find %s in status codes", status)
							}

							results = append(results, testrail.ResultsForCase{CaseID: mCaseID, SendableResult: testrail.SendableResult{StatusID: statusInt, Comment: comment}})
						}

						includeAll := false
						run, err := mirantisClient.AddRun(mirantisProjectID, testrail.SendableRun{SuiteID: mirantisSuiteID, Name: runName, CaseIDs: caseIDs, IncludeAll: &includeAll})
						if err != nil {
							log.Fatal(err)
						}
						fmt.Printf("Created run for %s on Mirantis Testrail\n", runName)

						_, err = mirantisClient.AddResultsForCases(run.ID, testrail.SendableResultsForCase{Results: results})
						if err != nil {
							log.Fatal(err)
						}
						fmt.Printf("Transfered results for %s in the Mirantis account\n", runName)

					}

					return nil
				})

				return nil
			},
		},
	}

	app.Run(os.Args)
}

func createClient(envPrefix, url string) (*testrail.Client, error) {
	username := os.Getenv(fmt.Sprintf("%sTESTRAIL_USERNAME", envPrefix))
	token := os.Getenv(fmt.Sprintf("%sTESTRAIL_TOKEN", envPrefix))

	if username == "" || token == "" {
		return nil, fmt.Errorf("Need to set TESTRAIL_USERNAME and TESTRAIL_TOKEN")
	}

	client := testrail.NewClient(url, username, token)

	return client, nil
}

// createMaps generates maps that serve as lookups between Docker testrail
// and Mirantis testrail
// maps are serialized and written to files to conserve api rate limits
func createMaps(dockerClient, mirantisClient *testrail.Client, dockerProjectID, dockerSuiteID, mirantisProjectID, mirantisSuiteID int) map[int]int {
	var dToMCaseIds map[int]int = make(map[int]int)
	var descToCaseIds map[string]int = make(map[string]int)

	var dSectionToDesc map[int]string = make(map[int]string)
	var mSectionToDesc map[int]string = make(map[int]string)
	var dockerDuplicates map[string][]int = make(map[string][]int)
	var mirantisDuplicates map[string][]int = make(map[string][]int)

	sections, err := dockerClient.GetSections(dockerProjectID, dockerSuiteID)
	if err != nil {
		log.Fatal(err)
	}
	for _, section := range sections {
		// for each section map the section ID to the name of the section
		if _, ok := dSectionToDesc[section.ID]; ok {
			log.Fatalf("Duplicate entry for section id with ID %s", section.ID)
		}
		dSectionToDesc[section.ID] = section.Name
	}

	// Encode the descToSectionIds map to a file
	//encodeMap(descToSectionIds, "descToSectionIds")

	sections, err = mirantisClient.GetSections(mirantisProjectID, mirantisSuiteID)
	if err != nil {
		log.Fatal(err)
	}
	for _, section := range sections {
		// for each section map the section ID to the name of the section
		if _, ok := mSectionToDesc[section.ID]; ok {
			log.Fatalf("Duplicate entry for section id with ID %s", section.ID)
		}
		mSectionToDesc[section.ID] = section.Name
	}

	// Encode the dToMSectionIDs map to a file
	//encodeMap(dToMSectionIds, "dToMSectionIds")

	// get all case ids for Docker testrail
	// and create map of section name + test desription to Test case id
	// get all the case ids for Mirantis Testrail look up the Docker
	// case id based on the description and create the docker id to Mirantis map
	cases, err := dockerClient.GetCases(dockerProjectID, dockerSuiteID)
	if err != nil {
		log.Fatal(err)
	}
	for _, c := range cases {
		var sectionName string
		var caseID int
		var ok bool
		if sectionName, ok = dSectionToDesc[c.SectionID]; !ok {
			log.Fatalf("%d not found in dSectionToDesc", c.SectionID)
		}
		key := fmt.Sprintf("%s_%s", sectionName, c.Title)

		if caseID, ok = descToCaseIds[key]; ok {
			log.Printf("Duplicate entry in dict with %s", key)
			if _, ok = dockerDuplicates[key]; !ok {
				// first time around add the prior case ID
				dockerDuplicates[key] = []int{caseID, c.ID}
			} else {
				dockerDuplicates[key] = append(dockerDuplicates[key], c.ID)
			}
		}
		descToCaseIds[key] = c.ID
	}

	// Encode the descToCaseIds map to a file
	//encodeMap(descToCaseIds, "descToCaseIds")

	cases, err = mirantisClient.GetCases(mirantisProjectID, mirantisSuiteID)
	if err != nil {
		log.Fatal(err)
	}
	for _, c := range cases {
		var sectionName string
		var ok bool
		if sectionName, ok = mSectionToDesc[c.SectionID]; !ok {
			log.Fatalf("%d not found in mSectionToDesc", c.SectionID)
		}
		key := fmt.Sprintf("%s_%s", sectionName, c.Title)

		// should only do look up if these values are unambiguous
		if _, ok = dockerDuplicates[key]; !ok {
			dToMCaseIds[descToCaseIds[key]] = c.ID
		} else {
			mirantisDuplicates[key] = append(mirantisDuplicates[key], c.ID)
		}
		//fmt.Printf("%s dockerID: %d, MirantisID: %d\n", c.Title, descToCaseIds[key], c.ID)
	}
	//fmt.Printf("\n\n\nMirantis Duplicates are %v", mirantisDuplicates)
	//fmt.Printf("\n\n\nDocker Duplicates are %v", dockerDuplicates)

	//fmt.Print("\n\n\nDocker duplicates are:\n ")

	// it turns out that the duplicate tests are slightly different
	// in Docker but this information is lost in the Mirantis testrail (frustrating)
	// to at least move forward the lower testrail ids in Docker testrails
	// for duplicate entries will map to lower testrail ids for Mirantis testrails
	//fmt.Printf("Length of map is %d\n", len(dToMCaseIds))
	for k, v := range dockerDuplicates {
		var ok bool
		var mirantisCases []int

		if mirantisCases, ok = mirantisDuplicates[k]; !ok {
			log.Fatal("Could not find %s in MirantisDuplicates", k)
		}

		mMin, mMax, err := returnMinMax(mirantisCases)
		if err != nil {
			log.Fatal(err)
		}

		dMin, dMax, err := returnMinMax(v)
		if err != nil {
			log.Fatal(err)
		}

		// sanity check to make sure key is not already in map
		if _, ok := dToMCaseIds[dMin]; ok {
			log.Fatalf("%d already in dToCaseIds", dMin)
		}
		dToMCaseIds[dMin] = mMin

		if _, ok := dToMCaseIds[dMax]; ok {
			log.Fatal("%d already in dToCaseIds", dMax)
		}
		dToMCaseIds[dMax] = mMax
	}

	//fmt.Print("\n\n\nMirantis duplicates are:\n ")
	//for k, v := range mirantisDuplicates {
	//	fmt.Printf("%s %v\n", k, v)
	//}

	// Encode the descToCaseIds map to a file
	//encodeMap(dToMCaseIds, "dToMCaseIds")
	//fmt.Printf("Length of map is %d\n", len(dToMCaseIds))

	return dToMCaseIds
}

// returnMinMax returns the min and max of a 2 element
// array and returns an error if the array is not 2 elements
// long
func returnMinMax(inputArray []int) (int, int, error) {
	if len(inputArray) != 2 {
		return 0, 0, fmt.Errorf("Array is not of length 2")
	}

	if inputArray[0] < inputArray[1] {
		return inputArray[0], inputArray[1], nil
	}
	return inputArray[1], inputArray[0], nil
}

func encodeMap(inputMap interface{}, mapName string) {
	encodeFile, err := os.Create(mapName)
	defer encodeFile.Close()
	if err != nil {
		log.Fatal(err)
	}
	encoder := gob.NewEncoder(encodeFile)
	if err := encoder.Encode(inputMap); err != nil {
		log.Fatal(err)
	}
}

// We only want to send the results if they are applicable for a given runID or the API will throw an error.
func pruneResults(client *testrail.Client, runID int, results testrail.SendableResultsForCase) (testrail.SendableResultsForCase, error) {
	// First gather all the cases of the runID
	tests, err := client.GetTests(runID)
	if err != nil {
		return testrail.SendableResultsForCase{}, err
	}

	// Create a map of these test case IDs
	includedTests := make(map[int]struct{})
	for _, test := range tests {
		includedTests[test.CaseID] = struct{}{}
	}

	var applicableResults testrail.SendableResultsForCase
	for _, result := range results.Results {
		if _, exists := includedTests[result.CaseID]; exists {
			applicableResults.Results = append(applicableResults.Results, result)
		}
	}
	return applicableResults, nil
}
