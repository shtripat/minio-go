/*
 * Minio Go Library for Amazon S3 Compatible Cloud Storage (C) 2015 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package minio

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GetBucketACL get the permissions on an existing bucket.
//
// Returned values are:
//
//  private - owner gets full access.
//  public-read - owner gets full access, others get read access.
//  public-read-write - owner gets full access, others get full access too.
//  authenticated-read - owner gets full access, authenticated users get read access.
func (c Client) GetBucketACL(bucketName string) (BucketACL, error) {
	if err := isValidBucketName(bucketName); err != nil {
		return "", err
	}

	// Set acl query.
	urlValues := make(url.Values)
	urlValues.Set("acl", "")

	// Instantiate a new request.
	req, err := c.newRequest("GET", requestMetadata{
		bucketName:  bucketName,
		queryValues: urlValues,
	})
	if err != nil {
		return "", err
	}

	// Initiate the request.
	resp, err := c.httpClient.Do(req)
	defer closeResponse(resp)
	if err != nil {
		return "", err
	}
	if resp != nil {
		if resp.StatusCode != http.StatusOK {
			return "", HTTPRespToErrorResponse(resp, bucketName, "")
		}
	}

	// Decode access control policy.
	policy := accessControlPolicy{}
	err = xmlDecoder(resp.Body, &policy)
	if err != nil {
		return "", err
	}

	// We need to avoid following de-serialization check for Google Cloud Storage.
	// On Google Cloud Storage "private" canned ACL's policy do not have grant list.
	// Treat it as a valid case, check for all other vendors.
	if !isGoogleEndpoint(c.endpointURL) {
		if policy.AccessControlList.Grant == nil {
			errorResponse := ErrorResponse{
				Code:            "InternalError",
				Message:         "Access control Grant list is empty. " + reportIssue,
				BucketName:      bucketName,
				RequestID:       resp.Header.Get("x-amz-request-id"),
				HostID:          resp.Header.Get("x-amz-id-2"),
				AmzBucketRegion: resp.Header.Get("x-amz-bucket-region"),
			}
			return "", errorResponse
		}
	}

	// boolean cues to indentify right canned acls.
	var publicRead, publicWrite bool

	// Handle grants.
	grants := policy.AccessControlList.Grant
	for _, g := range grants {
		if g.Grantee.URI == "" && g.Permission == "FULL_CONTROL" {
			continue
		}
		if g.Grantee.URI == "http://acs.amazonaws.com/groups/global/AuthenticatedUsers" && g.Permission == "READ" {
			return BucketACL("authenticated-read"), nil
		} else if g.Grantee.URI == "http://acs.amazonaws.com/groups/global/AllUsers" && g.Permission == "WRITE" {
			publicWrite = true
		} else if g.Grantee.URI == "http://acs.amazonaws.com/groups/global/AllUsers" && g.Permission == "READ" {
			publicRead = true
		}
	}

	// public write and not enabled. return.
	if !publicWrite && !publicRead {
		return BucketACL("private"), nil
	}
	// public write not enabled but public read is. return.
	if !publicWrite && publicRead {
		return BucketACL("public-read"), nil
	}
	// public read and public write are enabled return.
	if publicRead && publicWrite {
		return BucketACL("public-read-write"), nil
	}

	return "", ErrorResponse{
		Code:       "NoSuchBucketPolicy",
		Message:    "The specified bucket does not have a bucket policy.",
		BucketName: bucketName,
		RequestID:  "minio",
	}
}

// GetObject gets object content from specified bucket.
// You may also look at GetPartialObject.
func (c Client) GetObject(bucketName, objectName string) (io.ReadSeeker, error) {
	if err := isValidBucketName(bucketName); err != nil {
		return nil, err
	}
	if err := isValidObjectName(objectName); err != nil {
		return nil, err
	}
	// get object.
	return newObjectReadSeeker(c, bucketName, objectName), nil
}

// GetPartialObject gets partial object content as specified by the Range.
//
// Setting offset and length = 0 will download the full object.
// For more information about the HTTP Range header,
// go to http://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35
func (c Client) GetPartialObject(bucketName, objectName string, offset, length int64) (io.ReadSeeker, error) {
	if err := isValidBucketName(bucketName); err != nil {
		return nil, err
	}
	if err := isValidObjectName(objectName); err != nil {
		return nil, err
	}
	// get partial object.
	return newObjectReadSeeker(c, bucketName, objectName), nil
}

// objectReadSeeker container for io.ReadSeeker.
type objectReadSeeker struct {
	// mutex.
	mutex *sync.Mutex

	client     Client
	reader     io.ReadCloser
	isRead     bool
	stat       ObjectStat
	offset     int64
	bucketName string
	objectName string
}

// newObjectReadSeeker wraps getObject request returning a io.ReadSeeker.
func newObjectReadSeeker(client Client, bucket, object string) *objectReadSeeker {
	return &objectReadSeeker{
		mutex:      new(sync.Mutex),
		reader:     nil,
		isRead:     false,
		client:     client,
		offset:     0,
		bucketName: bucket,
		objectName: object,
	}
}

// Read reads up to len(p) bytes into p.  It returns the number of bytes
// read (0 <= n <= len(p)) and any error encountered.  Even if Read
// returns n < len(p), it may use all of p as scratch space during the call.
// If some data is available but not len(p) bytes, Read conventionally
// returns what is available instead of waiting for more.
//
// When Read encounters an error or end-of-file condition after
// successfully reading n > 0 bytes, it returns the number of
// bytes read.  It may return the (non-nil) error from the same call
// or return the error (and n == 0) from a subsequent call.
// An instance of this general case is that a Reader returning
// a non-zero number of bytes at the end of the input stream may
// return either err == EOF or err == nil.  The next Read should
// return 0, EOF.
func (r *objectReadSeeker) Read(p []byte) (int, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if !r.isRead {
		reader, _, err := r.client.getObject(r.bucketName, r.objectName, r.offset, 0)
		if err != nil {
			return 0, err
		}
		r.reader = reader
		r.isRead = true
	}
	n, err := r.reader.Read(p)
	if err == io.EOF {
		// drain any remaining body, discard it before closing the body.
		io.Copy(ioutil.Discard, r.reader)
		r.reader.Close()
		return n, err
	}
	if err != nil {
		// drain any remaining body, discard it before closing the body.
		io.Copy(ioutil.Discard, r.reader)
		r.reader.Close()
		return 0, err
	}
	return n, nil
}

// Seek sets the offset for the next Read or Write to offset,
// interpreted according to whence: 0 means relative to the start of
// the file, 1 means relative to the current offset, and 2 means
// relative to the end. Seek returns the new offset relative to the
// start of the file and an error, if any.
//
// Seeking to an offset before the start of the file is an error.
// TODO: whence value of '1' and '2' are not implemented yet.
func (r *objectReadSeeker) Seek(offset int64, whence int) (int64, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.offset = offset
	return offset, nil
}

// Size returns the size of the object.
func (r *objectReadSeeker) Size() (int64, error) {
	objectSt, err := r.client.StatObject(r.bucketName, r.objectName)
	r.stat = objectSt
	return r.stat.Size, err
}

// getObject - retrieve object from Object Storage.
//
// Additionally this function also takes range arguments to download the specified
// range bytes of an object. Setting offset and length = 0 will download the full object.
//
// For more information about the HTTP Range header.
// go to http://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35.
func (c Client) getObject(bucketName, objectName string, offset, length int64) (io.ReadCloser, ObjectStat, error) {
	// Validate input arguments.
	if err := isValidBucketName(bucketName); err != nil {
		return nil, ObjectStat{}, err
	}
	if err := isValidObjectName(objectName); err != nil {
		return nil, ObjectStat{}, err
	}

	customHeader := make(http.Header)
	// Set ranges if length and offset are valid.
	if length > 0 && offset >= 0 {
		customHeader.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	} else if offset > 0 && length == 0 {
		customHeader.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	} else if length < 0 && offset == 0 {
		customHeader.Set("Range", fmt.Sprintf("bytes=%d", length))
	}

	// Instantiate a new request.
	req, err := c.newRequest("GET", requestMetadata{
		bucketName:   bucketName,
		objectName:   objectName,
		customHeader: customHeader,
	})
	if err != nil {
		return nil, ObjectStat{}, err
	}
	// Execute the request.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, ObjectStat{}, err
	}
	if resp != nil {
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			return nil, ObjectStat{}, HTTPRespToErrorResponse(resp, bucketName, objectName)
		}
	}
	// trim off the odd double quotes.
	md5sum := strings.Trim(resp.Header.Get("ETag"), "\"")
	// parse the date.
	date, err := time.Parse(http.TimeFormat, resp.Header.Get("Last-Modified"))
	if err != nil {
		msg := "Last-Modified time format not recognized. " + reportIssue
		return nil, ObjectStat{}, ErrorResponse{
			Code:            "InternalError",
			Message:         msg,
			RequestID:       resp.Header.Get("x-amz-request-id"),
			HostID:          resp.Header.Get("x-amz-id-2"),
			AmzBucketRegion: resp.Header.Get("x-amz-bucket-region"),
		}
	}
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	var objectStat ObjectStat
	objectStat.ETag = md5sum
	objectStat.Key = objectName
	objectStat.Size = resp.ContentLength
	objectStat.LastModified = date
	objectStat.ContentType = contentType

	// do not close body here, caller will close
	return resp.Body, objectStat, nil
}