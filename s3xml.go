package main

import (
	"encoding/xml"
	"net/http"
	"time"
)

const s3NS = "http://s3.amazonaws.com/doc/2006-03-01/"

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type bucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type listAllMyBucketsResult struct {
	XMLName xml.Name      `xml:"ListAllMyBucketsResult"`
	Xmlns   string        `xml:"xmlns,attr"`
	Owner   owner         `xml:"Owner"`
	Buckets []bucketEntry `xml:"Buckets>Bucket"`
}

type objectEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type listBucketResult struct {
	XMLName              xml.Name       `xml:"ListBucketResult"`
	Xmlns                string         `xml:"xmlns,attr"`
	Name                 string         `xml:"Name"`
	Prefix               string         `xml:"Prefix"`
	Delimiter            string         `xml:"Delimiter,omitempty"`
	KeyCount             int            `xml:"KeyCount"`
	MaxKeys              int            `xml:"MaxKeys"`
	IsTruncated          bool           `xml:"IsTruncated"`
	Contents             []objectEntry  `xml:"Contents,omitempty"`
	CommonPrefixes       []commonPrefix `xml:"CommonPrefixes,omitempty"`
	ContinuationToken    string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
}

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type copyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	Xmlns        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

type multipartUploadEntry struct {
	Key          string `xml:"Key"`
	UploadID     string `xml:"UploadId"`
	Initiated    string `xml:"Initiated"`
	StorageClass string `xml:"StorageClass"`
}

type listMultipartUploadsResult struct {
	XMLName            xml.Name              `xml:"ListMultipartUploadsResult"`
	Xmlns              string                `xml:"xmlns,attr"`
	Bucket             string                `xml:"Bucket"`
	KeyMarker          string                `xml:"KeyMarker,omitempty"`
	UploadIDMarker     string                `xml:"UploadIdMarker,omitempty"`
	NextKeyMarker      string                `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string                `xml:"NextUploadIdMarker,omitempty"`
	MaxUploads         int                   `xml:"MaxUploads"`
	IsTruncated        bool                  `xml:"IsTruncated"`
	Uploads            []multipartUploadEntry `xml:"Upload,omitempty"`
}

type partEntry struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

type listPartsResult struct {
	XMLName              xml.Name    `xml:"ListPartsResult"`
	Xmlns                string      `xml:"xmlns,attr"`
	Bucket               string      `xml:"Bucket"`
	Key                  string      `xml:"Key"`
	UploadID             string      `xml:"UploadId"`
	PartNumberMarker     int         `xml:"PartNumberMarker,omitempty"`
	NextPartNumberMarker int         `xml:"NextPartNumberMarker,omitempty"`
	MaxParts             int         `xml:"MaxParts"`
	IsTruncated          bool        `xml:"IsTruncated"`
	Parts                []partEntry `xml:"Part,omitempty"`
}

type locationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
	Text    string   `xml:",chardata"`
}

type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr"`
}

type objectLockConfiguration struct {
	XMLName            xml.Name `xml:"ObjectLockConfiguration"`
	Xmlns              string   `xml:"xmlns,attr"`
	ObjectLockEnabled  string   `xml:"ObjectLockEnabled"`
}

func writeXML(w http.ResponseWriter, status int, v interface{}) {
	data, err := xml.Marshal(v)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	w.Write([]byte(xml.Header))
	w.Write(data)
}

func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
