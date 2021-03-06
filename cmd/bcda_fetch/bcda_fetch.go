// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// fetch is an example program that uses the bcda API client library to retrieve
// resources from the BCDA API.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"flag"
	log "github.com/golang/glog"
	"github.com/google/medical_claims_tools/bcda"
	"github.com/google/medical_claims_tools/fhir"
	"github.com/google/medical_claims_tools/fhirstore"
	"github.com/google/medical_claims_tools/internal/counter"
)

var (
	clientID                    = flag.String("client_id", "", "BCDA API client ID (required)")
	clientSecret                = flag.String("client_secret", "", "BCDA API client secret (required)")
	outputPrefix                = flag.String("output_prefix", "", "Data output prefix. If unset, no file output will be written.")
	useV2                       = flag.Bool("use_v2", false, "This indicates if the BCDA V2 API should be used, which returns R4 mapped data.")
	rectify                     = flag.Bool("rectify", false, "This indicates that this program should attempt to rectify BCDA FHIR so that it is valid R4 FHIR. This is needed for FHIR store upload.")
	enableFHIRStore             = flag.Bool("enable_fhir_store", false, "If true, this enables write to GCP FHIR store. If true, all other fhir_store_* flags and the rectify flag must be set.")
	maxFHIRStoreUploadWorkers   = flag.Int("max_fhir_store_upload_workers", 10, "The max number of concurrent FHIR store upload workers.")
	fhirStoreGCPProject         = flag.String("fhir_store_gcp_project", "", "The GCP project for the FHIR store to upload to.")
	fhirStoreGCPLocation        = flag.String("fhir_store_gcp_location", "", "The GCP location of the FHIR Store.")
	fhirStoreGCPDatasetID       = flag.String("fhir_store_gcp_dataset_id", "", "The dataset ID for the FHIR Store.")
	fhirStoreID                 = flag.String("fhir_store_id", "", "The FHIR Store ID.")
	fhirStoreUploadErrorFileDir = flag.String("fhir_store_upload_error_file_dir", "", "An optional path to a directory where an upload errors file should be written. This file will contain the FHIR NDJSON and error information of FHIR resources that fail to upload to FHIR store.")
	serverURL                   = flag.String("bcda_server_url", "https://sandbox.bcda.cms.gov", "The BCDA server to communicate with. By deafult this is https://sandbox.bcda.cms.gov")
	since                       = flag.String("since", "", "The optional timestamp after which data should be fetched for. If not specified, fetches all available data. This should be a FHIR instant in the form of YYYY-MM-DDThh:mm:ss.sss+zz:zz.")
	sinceFile                   = flag.String("since_file", "", "Optional. If specified, the fetch program will read the latest since timestamp in this file to use when fetching data from BCDA. DO NOT run simultaneous fetch programs with the same since file. Once the fetch is completed successfully, fetch will write the BCDA transaction timestamp for this fetch operation to the end of the file specified here, to be used in the subsequent run (to only fetch new data since the last successful run). The first time fetch is run with this flag set, it will fetch all data.")
	noFailOnUploadErrors        = flag.Bool("no_fail_on_upload_errors", false, "If true, fetch will not fail on FHIR store upload errors, and will continue (and write out updates to since_file) as normal.")
	bcdaJobID                   = flag.String("bcda_job_id", "", "If set, skip calling the BCD API to create a new data export job. Instead, bcda_fetch will download and process the data from the BCDA job ID provided by this flag. bcda_fetch will wait until the provided job id is complete before proceeding.")
)

// Note that counters are initialized in mainWrapper.

// fhirStoreUploadErrorCounter is a counter for FHIR store upload failures.
var fhirStoreUploadErrorCounter *counter.Counter

var (
	errInvalidSince            = errors.New("invalid since timestamp")
	errUploadFailures          = errors.New("fhir store upload failures")
	errMustRectifyForFHIRStore = errors.New("for now, rectify must be enabled for FHIR store upload")
)

const (
	// jobStatusPeriod indicates how often we check a pending job status.
	jobStatusPeriod = 5 * time.Second
	// jobStatusTimeout indicates the maximum time that should be spend checking on
	// a pending JobStatus.
	jobStatusTimeout = 6 * time.Hour
	// maxTokenSize represents the maximum newline delimited token size in bytes
	// expected when parsing FHIR NDJSON.
	maxTokenSize = 500 * 1024
	// initialBufferSize indicates the initial buffer size in bytes to use when
	// parsing a FHIR NDJSON token.
	initialBufferSize = 5 * 1024
)

func main() {
	flag.Parse()
	if err := mainWrapper(defaultMainWrapperConfig()); err != nil {
		log.Fatal(err)
	}
}

// mainWrapper allows for easier testing of the main function.
func mainWrapper(cfg mainWrapperConfig) error {
	// Init counters
	fhirStoreUploadErrorCounter = counter.New()

	if *clientID == "" || *clientSecret == "" {
		return errors.New("both clientID and clientSecret flags must be non-empty")
	}

	if *enableFHIRStore && (*fhirStoreGCPProject == "" ||
		*fhirStoreGCPLocation == "" ||
		*fhirStoreGCPDatasetID == "" ||
		*fhirStoreID == "") {
		return errors.New("if enable_fhir_store is true, all other FHIR store related flags must be set")
	}

	if *enableFHIRStore && !*rectify {
		return errMustRectifyForFHIRStore
	}

	if *outputPrefix == "" && !*enableFHIRStore {
		log.Warningln("outputPrefix is not set and neither is enableFHIRStore: BCDA fetch will not produce any output.")
	}

	apiVersion := bcda.V1
	if *useV2 {
		apiVersion = bcda.V2
	}

	cl, err := bcda.NewClient(*serverURL, apiVersion)
	if err != nil {
		return fmt.Errorf("NewClient(%v, %v) error: %v", serverURL, apiVersion, err)
	}

	_, err = cl.Authenticate(*clientID, *clientSecret)
	if err != nil {
		return fmt.Errorf("Error authenticating with API: %v", err)
	}

	parsedSince, err := getSince()
	if err != nil {
		return err
	}

	jobID := *bcdaJobID
	if *bcdaJobID == "" {
		jobID, err = cl.StartBulkDataExport(bcda.AllResourceTypes, parsedSince)
		if err != nil {
			return fmt.Errorf("unable to StartBulkDataExport: %v", err)
		}
		log.Infof("Started BCDA job: %s\n", jobID)
	}

	var jobStatus bcda.JobStatus
	iter := 0
	// Check the BCDA job status until it is complete or until the time spent
	// checking exceeds the timeout.
	for !jobStatus.IsComplete && (jobStatusPeriod*time.Duration(iter)) < jobStatusTimeout {
		jobStatus, err = cl.JobStatus(jobID)
		if err != nil && err != bcda.ErrorUnableToParseProgress && err != bcda.ErrorUnauthorized {
			return fmt.Errorf("error getting job status: %v", err)
		}

		if err == bcda.ErrorUnauthorized {
			_, err = cl.Authenticate(*clientID, *clientSecret)
			if err != nil {
				return fmt.Errorf("Error authenticating with API: %v", err)
			}
			continue
		}

		if !jobStatus.IsComplete {
			iter++
			log.Infof("BCDA Export job pending, progress: %d\n", jobStatus.PercentComplete)
			time.Sleep(jobStatusPeriod)
		}
	}

	if !jobStatus.IsComplete {
		return fmt.Errorf("BCDA Job did not finish before the timeout of %v", jobStatusTimeout)
	}

	log.Infof("BCDA Job Finished. Transaction Time: %v", fhir.ToFHIRInstant(jobStatus.TransactionTime))
	log.Infof("Begin BCDA data download and write out to disk.")
	if *enableFHIRStore {
		log.Infof("Data will also be uploaded to FHIR store based on provided parmaters.")
	}

	for r, urls := range jobStatus.ResultURLs {
		for i, url := range urls {
			filePrefix := ""
			if *outputPrefix != "" {
				filePrefix = fmt.Sprintf("%s_%s_%d", *outputPrefix, r, i)
			}

			r, err := getDataOrExit(cl, url, *clientID, *clientSecret)
			if err != nil {
				return err
			}
			defer r.Close()
			if *rectify {
				rectifyAndWrite(r, filePrefix, cfg)
			} else {
				writeData(r, filePrefix)
			}
		}
	}

	// Check failure counters.
	if errCnt := fhirStoreUploadErrorCounter.CloseAndGetCount(); errCnt > 0 {
		log.Warningf("non-zero FHIR Store Upload Errors: %d", errCnt)
		if !*noFailOnUploadErrors {
			return fmt.Errorf("non-zero FHIR store upload errors (check logs for details): %d %w", errCnt, errUploadFailures)
		}
	}

	// Write out since file time. This should only be written if there are no
	// errors during the fetch process.
	if *sinceFile != "" {
		f, err := os.OpenFile(*sinceFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		defer f.Close()
		f.Write([]byte(fhir.ToFHIRInstant(jobStatus.TransactionTime) + "\n"))
	}

	log.Info("bcda_fetch complete.")
	return nil
}

func getDataOrExit(cl *bcda.Client, url, clientID, clientSecret string) (io.ReadCloser, error) {
	r, err := cl.GetData(url)
	numRetries := 0
	// Retry both unauthorized and other retryable errors by re-authenticating,
	// as sometimes they appear to be related.
	for (errors.Is(err, bcda.ErrorUnauthorized) || errors.Is(err, bcda.ErrorRetryableHTTPStatus)) && numRetries < 5 {
		time.Sleep(2 * time.Second)
		log.Infof("Got retryable error from BCDA. Re-authenticating and trying again.")
		if _, err := cl.Authenticate(clientID, clientSecret); err != nil {
			return nil, fmt.Errorf("Error authenticating with API: %w", err)
		}
		r, err = cl.GetData(url)
		numRetries++
	}

	if err != nil {
		return nil, fmt.Errorf("Unable to GetData(%s) %w", url, err)
	}

	return r, nil
}

func writeData(r io.Reader, filePrefix string) {
	f, err := os.OpenFile(fmt.Sprintf("%s.ndjson", filePrefix), os.O_RDWR|os.O_CREATE, 0755)
	if err != nil {
		log.Exitf("Unable to create output file. Error: %v", err)
	}
	defer f.Close()

	_, err = io.Copy(f, r)
	if err != nil {
		log.Exitf("Unable to copy data to output file. Error: %v", err)
	}
}

func rectifyAndWrite(r io.Reader, filePrefix string, cfg mainWrapperConfig) {
	var w io.WriteCloser = nil
	if filePrefix != "" {
		var err error
		w, err = os.OpenFile(fmt.Sprintf("%s.ndjson", filePrefix), os.O_RDWR|os.O_CREATE, 0755)
		if err != nil {
			log.Exitf("Unable to create output file: %v", err)
		}
		defer w.Close()
	}

	uploader, err := fhirstore.NewUploader(fhirstore.UploaderConfig{
		FHIRStoreEndpoint:   cfg.fhirStoreEndpoint,
		FHIRStoreID:         *fhirStoreID,
		FHIRProjectID:       *fhirStoreGCPProject,
		FHIRLocation:        *fhirStoreGCPLocation,
		FHIRDatasetID:       *fhirStoreGCPDatasetID,
		MaxWorkers:          *maxFHIRStoreUploadWorkers,
		ErrorCounter:        fhirStoreUploadErrorCounter,
		ErrorFileOutputPath: *fhirStoreUploadErrorFileDir,
	})

	if err != nil {
		log.Exitf("unable to init uploader: %v", err)
	}

	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, initialBufferSize), maxTokenSize)
	for s.Scan() {
		fhirOut, err := fhir.RectifyBCDA(s.Bytes())
		if err != nil {
			log.Warningf("WARN: issue during rectification: %v, proceeding without rectification for this resource.", err)
			// Override fhirOut to be the original un-rectified json.
			fhirOut = s.Bytes()
		}

		// Add this FHIR resource to the queue to be written to FHIR store.
		if *enableFHIRStore {
			uploader.Upload(fhirOut)
		}

		if w != nil {
			// We write with a newline because this is a newline delimited JSON file.
			if _, err := w.Write(append(fhirOut, byte('\n'))); err != nil {
				log.Exitf("issue during file write: %v", err)
			}
		}
	}
	// Since there is only one sender goroutine (this one), it should be safe to
	// indicate we're done sending here, after we have finished sending all the
	// work in the for loop above.
	if *enableFHIRStore {
		uploader.DoneUploading()
		if err := uploader.Wait(); err != nil {
			log.Warningf("error closing the uploader: %v", err)
		}
	}
}

func getSince() (time.Time, error) {
	parsedSince := time.Time{}
	if *since != "" && *sinceFile != "" {
		return parsedSince, errors.New("only one of since or since_file flags may be set (cannot set both)")
	}

	var err error
	if *since != "" {
		parsedSince, err = fhir.ParseFHIRInstant(*since)
		if err != nil {
			return parsedSince, fmt.Errorf("invalid since timestamp provided (%s), should be in form YYYY-MM-DDThh:mm:ss.sss+zz:zz %w", *since, errInvalidSince)
		}
	}

	if *sinceFile != "" {
		if info, err := os.Stat(*sinceFile); errors.Is(err, os.ErrNotExist) || (err == nil && info.Size() == 0) {
			// There is no since information, since this is the first time fetch is being
			// called on a new file or an empty file--so we return an empty time to fetch
			// all data the first time.
			return time.Time{}, nil
		}

		f, err := os.Open(*sinceFile)
		if err != nil {
			return parsedSince, err
		}
		defer f.Close()

		// TODO(b/213614828): if we expect extremely large since files in the future,
		// we can make this more efficient by reading the file backwards (at the
		// expense of more complex code).
		s := bufio.NewScanner(f)
		lastLine := ""
		for s.Scan() {
			lastLine = s.Text()
		} // Scan through all lines of the file
		parsedSince, err = fhir.ParseFHIRInstant(lastLine)
		if err != nil {
			return parsedSince, fmt.Errorf("invalid since timestamp provided (%s), should be in form YYYY-MM-DDThh:mm:ss.sss+zz:zz %w", lastLine, errInvalidSince)
		}
	}

	return parsedSince, nil
}

// mainWrapperConfig holds non-flag (for now) config variables for the
// mainWrapper function. This is largely to assist in better testing without
// having to change global variables.
// TODO(b/213587622): it may be possible to safely refactor flags into this
// struct in the future.
type mainWrapperConfig struct {
	fhirStoreEndpoint string
}

func defaultMainWrapperConfig() mainWrapperConfig {
	return mainWrapperConfig{
		fhirStoreEndpoint: fhirstore.DefaultHealthcareEndpoint,
	}
}
