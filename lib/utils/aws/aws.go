/*
Copyright 2021 Gravitational, Inc.

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

package aws

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/aws/credentials"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"

	apievents "github.com/gravitational/teleport/api/types/events"
	apiawsutils "github.com/gravitational/teleport/api/utils/aws"
	"github.com/gravitational/teleport/lib/utils"
)

const (
	// AmazonSigV4AuthorizationPrefix is AWS Authorization prefix indicating that the request
	// was signed by AWS Signature Version 4.
	// https://github.com/aws/aws-sdk-go/blob/main/aws/signer/v4/v4.go#L83
	// https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-auth-using-authorization-header.html
	AmazonSigV4AuthorizationPrefix = "AWS4-HMAC-SHA256"

	// AmzDateTimeFormat is time format used in X-Amz-Date header.
	// https://github.com/aws/aws-sdk-go/blob/main/aws/signer/v4/v4.go#L84
	AmzDateTimeFormat = "20060102T150405Z"

	// AmzDateHeader is header name containing timestamp when signature was generated.
	// https://docs.aws.amazon.com/general/latest/gr/sigv4-date-handling.html
	AmzDateHeader = "X-Amz-Date"

	AuthorizationHeader        = "Authorization"
	credentialAuthHeaderElem   = "Credential"
	signedHeaderAuthHeaderElem = "SignedHeaders"
	signatureAuthHeaderElem    = "Signature"

	// AmzTargetHeader is a header containing the API target.
	// Format: target_version.operation
	// Example: DynamoDB_20120810.Scan
	AmzTargetHeader = "X-Amz-Target"
	// AmzJSON1_0 is an AWS Content-Type header that indicates the media type is JSON.
	AmzJSON1_0 = "application/x-amz-json-1.0"
	// AmzJSON1_1 is an AWS Content-Type header that indicates the media type is JSON.
	AmzJSON1_1 = "application/x-amz-json-1.1"
)

// SigV4 contains parsed content of the AWS Authorization header.
type SigV4 struct {
	// KeyIS is an AWS access-key-id
	KeyID string
	// Date value is specified using YYYYMMDD format.
	Date string
	// Region is an AWS Region.
	Region string
	// Service is an AWS Service.
	Service string
	// SignedHeaders is a  list of request headers that you used to compute Signature.
	SignedHeaders []string
	// Signature is the 256-bit Signature of the request.
	Signature string
}

// ParseSigV4 AWS SigV4 credentials string sections.
// AWS SigV4 header example:
// Authorization: AWS4-HMAC-SHA256
// Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,
// SignedHeaders=host;range;x-amz-date,
// Signature=fe5f80f77d5fa3beca038a248ff027d0445342fe2855ddc963176630326f1024
func ParseSigV4(header string) (*SigV4, error) {
	if header == "" {
		return nil, trace.BadParameter("empty AWS SigV4 header")
	}
	sectionParts := strings.Split(header, " ")

	m := make(map[string]string)
	for _, v := range sectionParts {
		kv := strings.Split(v, "=")
		if len(kv) != 2 {
			continue
		}
		m[kv[0]] = strings.TrimSuffix(kv[1], ",")
	}

	authParts := strings.Split(m[credentialAuthHeaderElem], "/")
	if len(authParts) != 5 {
		return nil, trace.BadParameter("invalid size of %q section", credentialAuthHeaderElem)
	}

	signature := m[signatureAuthHeaderElem]
	if signature == "" {
		return nil, trace.BadParameter("invalid signature")
	}
	var signedHeaders []string
	if v := m[signedHeaderAuthHeaderElem]; v != "" {
		signedHeaders = strings.Split(v, ";")
	}

	return &SigV4{
		KeyID:     authParts[0],
		Date:      authParts[1],
		Region:    authParts[2],
		Service:   authParts[3],
		Signature: signature,
		// Split semicolon-separated list of signed headers string.
		// https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-auth-using-authorization-header.html
		// https://github.com/aws/aws-sdk-go/blob/main/aws/signer/v4/v4.go#L631
		SignedHeaders: signedHeaders,
	}, nil
}

// IsSignedByAWSSigV4 checks is the request was signed by AWS Signature Version 4 algorithm.
// https://docs.aws.amazon.com/general/latest/gr/signing_aws_api_requests.html
func IsSignedByAWSSigV4(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get(AuthorizationHeader), AmazonSigV4AuthorizationPrefix)
}

// VerifyAWSSignature verifies the request signature ensuring that the request originates from tsh aws command execution
// AWS CLI signs the request with random generated credentials that are passed to LocalProxy by
// the AWSCredentials LocalProxyConfig configuration.
func VerifyAWSSignature(req *http.Request, credentials *credentials.Credentials) error {
	sigV4, err := ParseSigV4(req.Header.Get("Authorization"))
	if err != nil {
		return trace.BadParameter(err.Error())
	}

	// Verifies the request is signed by the expected access key ID.
	credValue, err := credentials.Get()
	if err != nil {
		return trace.Wrap(err)
	}

	if sigV4.KeyID != credValue.AccessKeyID {
		return trace.AccessDenied("AccessKeyID does not match")
	}

	// Skip signature verification if the incoming request includes the
	// "User-Agent" header when making the signature. AWS Go SDK explicitly
	// skips the "User-Agent" header so it will always produce a different
	// signature. Only AccessKeyID is verified above in this case.
	for _, signedHeader := range sigV4.SignedHeaders {
		if strings.EqualFold(signedHeader, "User-Agent") {
			return nil
		}
	}

	// Read the request body and replace the body ready with a new reader that will allow reading the body again
	// by HTTP Transport.
	payload, err := utils.GetAndReplaceRequestBody(req)
	if err != nil {
		return trace.Wrap(err)
	}

	reqCopy := req.Clone(context.Background())

	// Remove all the headers that are not present in awsCred.SignedHeaders.
	filterHeaders(reqCopy, sigV4.SignedHeaders)

	// Get the date that was used to create the signature of the original request
	// originated from AWS CLI and reuse it as a timestamp during request signing call.
	t, err := time.Parse(AmzDateTimeFormat, reqCopy.Header.Get(AmzDateHeader))
	if err != nil {
		return trace.BadParameter(err.Error())
	}

	signer := NewSigner(credentials, sigV4.Service)
	_, err = signer.Sign(reqCopy, bytes.NewReader(payload), sigV4.Service, sigV4.Region, t)
	if err != nil {
		return trace.Wrap(err)
	}

	localSigV4, err := ParseSigV4(reqCopy.Header.Get("Authorization"))
	if err != nil {
		return trace.Wrap(err)
	}

	// Compare the origin request AWS SigV4 signature with the signature calculated in LocalProxy based on
	// AWSCredentials taken from LocalProxyConfig.
	if sigV4.Signature != localSigV4.Signature {
		return trace.AccessDenied("signature verification failed")
	}
	return nil
}

// NewSigner creates a new V4 signer.
func NewSigner(credentials *credentials.Credentials, signingServiceName string) *v4.Signer {
	options := func(s *v4.Signer) {
		// s3 and s3control requests are signed with URL unescaped (found by
		// searching "DisableURIPathEscaping" in "aws-sdk-go/service"). Both
		// services use "s3" as signing name. See description of
		// "DisableURIPathEscaping" for more details.
		if signingServiceName == "s3" {
			s.DisableURIPathEscaping = true
		}
	}
	return v4.NewSigner(credentials, options)
}

// filterHeaders removes request headers that are not in the headers list and returns the removed header keys.
func filterHeaders(r *http.Request, headers []string) []string {
	keep := make(map[string]struct{})
	for _, key := range headers {
		keep[textproto.CanonicalMIMEHeaderKey(key)] = struct{}{}
	}

	var removed []string
	out := make(http.Header)
	for key, vals := range r.Header {
		if _, ok := keep[textproto.CanonicalMIMEHeaderKey(key)]; ok {
			out[key] = vals
			continue
		}
		removed = append(removed, key)
	}
	r.Header = out
	return removed
}

// FilterAWSRoles returns role ARNs from the provided list that belong to the
// specified AWS account ID.
//
// If AWS account ID is empty, all valid AWS IAM roles are returned.
func FilterAWSRoles(arns []string, accountID string) (result Roles) {
	for _, roleARN := range arns {
		parsed, err := ParseRoleARN(roleARN)
		if err != nil {
			logrus.Warnf("skipping invalid AWS role ARN: %v", err)
			continue
		}
		if accountID != "" && parsed.AccountID != accountID {
			continue
		}

		// In AWS convention, the display of the role is the last
		// /-delineated substring.
		//
		// Example ARNs:
		// arn:aws:iam::1234567890:role/EC2FullAccess      (display: EC2FullAccess)
		// arn:aws:iam::1234567890:role/path/to/customrole (display: customrole)
		parts := strings.Split(parsed.Resource, "/")
		result = append(result, Role{
			Name:    strings.Join(parts[1:], "/"),
			Display: parts[len(parts)-1],
			ARN:     roleARN,
		})
	}
	return result
}

// Role describes an AWS IAM role for AWS console access.
type Role struct {
	// Name is the full role name with the entire path.
	Name string `json:"name"`
	// Display is the role display name.
	Display string `json:"display"`
	// ARN is the full role ARN.
	ARN string `json:"arn"`
}

// Roles is a slice of roles.
type Roles []Role

// Sort sorts the roles by their display names.
func (roles Roles) Sort() {
	sort.SliceStable(roles, func(x, y int) bool {
		return strings.ToLower(roles[x].Display) < strings.ToLower(roles[y].Display)
	})
}

// FindRoleByARN finds the role with the provided ARN.
func (roles Roles) FindRoleByARN(arn string) (Role, bool) {
	for _, role := range roles {
		if role.ARN == arn {
			return role, true
		}
	}
	return Role{}, false
}

// FindRolesByName finds all roles matching the provided name.
func (roles Roles) FindRolesByName(name string) (result Roles) {
	for _, role := range roles {
		// Match either full name or the display name.
		if role.Display == name || role.Name == name {
			result = append(result, role)
		}
	}
	return
}

// UnmarshalRequestBody reads and unmarshals a JSON request body into a protobuf Struct wrapper.
// If the request is not a recognized AWS JSON media type, or the body cannot be read, or the body
// is not valid JSON, then this function returns a nil value and an error.
// The protobuf Struct wrapper is useful for serializing JSON into a protobuf, because otherwise when the
// protobuf is marshaled it will re-marshall a JSON string field with escape characters or base64 encode
// a []byte field.
// Examples showing differences:
// - JSON string in proto: `{"Table": "some-table"}` --marshal to JSON--> `"{\"Table\": \"some-table\"}"`
// - bytes in proto: []byte --marshal to JSON--> `eyJUYWJsZSI6ICJzb21lLXRhYmxlIn0K` (base64 encoded)
// - *Struct in proto: *Struct --marshal to JSON--> `{"Table": "some-table"}` (unescaped JSON)
func UnmarshalRequestBody(req *http.Request) (*apievents.Struct, error) {
	contentType := req.Header.Get("Content-Type")
	if !isJSON(contentType) {
		return nil, trace.BadParameter("invalid JSON request Content-Type: %q", contentType)
	}
	jsonBody, err := utils.GetAndReplaceRequestBody(req)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	s := &apievents.Struct{}
	if err := s.UnmarshalJSON(jsonBody); err != nil {
		return nil, trace.Wrap(err)
	}
	return s, nil
}

// isJSON returns true if the Content-Type is recognized as standard JSON or any non-standard
// Amazon Content-Type header that indicates JSON media type.
func isJSON(contentType string) bool {
	switch contentType {
	case "application/json", AmzJSON1_0, AmzJSON1_1:
		return true
	default:
		return false
	}
}

// BuildRoleARN constructs a string AWS ARN from a username, region, and account ID.
// If username is an AWS ARN, this function checks that the ARN is an AWS IAM Role ARN
// in the correct partition and account.
func BuildRoleARN(username, region, accountID string) (string, error) {
	partition := apiawsutils.GetPartitionFromRegion(region)
	if arn.IsARN(username) {
		// sanity check the given username role ARN.
		parsed, err := ParseRoleARN(username)
		if err != nil {
			return "", trace.Wrap(err)
		}
		// don't check for empty accountID - callers do not always pass an account ID,
		// and it's only absolutely required if we need to build the role ARN below.
		if err := CheckARNPartitionAndAccount(parsed, partition, accountID); err != nil {
			return "", trace.Wrap(err)
		}
		return username, nil
	}
	resource := username
	if !IsPartialRoleARN(resource) {
		resource = fmt.Sprintf("role/%s", username)
	}
	roleARN := &arn.ARN{
		Partition: partition,
		Service:   iam.ServiceName,
		AccountID: accountID,
		Resource:  resource,
	}
	if err := checkRoleARN(roleARN); err != nil {
		return "", trace.Wrap(err)
	}
	return roleARN.String(), nil
}

// ValidateRoleARNAndExtractRoleName validates the role ARN and extracts the
// short role name from it.
func ValidateRoleARNAndExtractRoleName(roleARN, wantPartition, wantAccountID string) (string, error) {
	role, err := ParseRoleARN(roleARN)
	if err != nil {
		return "", trace.Wrap(err)
	}
	if err := CheckARNPartitionAndAccount(role, wantPartition, wantAccountID); err != nil {
		return "", trace.Wrap(err)
	}
	return strings.TrimPrefix(role.Resource, "role/"), nil
}

// ParseRoleARN parses an AWS ARN and checks that the ARN is
// for an IAM Role resource.
func ParseRoleARN(roleARN string) (*arn.ARN, error) {
	role, err := arn.Parse(roleARN)
	if err != nil {
		return nil, trace.BadParameter("invalid AWS ARN: %v", err)
	}
	if err := checkRoleARN(&role); err != nil {
		return nil, trace.Wrap(err)
	}
	return &role, nil
}

// checkRoleARN returns whether a parsed ARN is for an IAM Role resource.
// Example role ARN: arn:aws:iam::123456789012:role/some-role-name
func checkRoleARN(parsed *arn.ARN) error {
	parts := strings.Split(parsed.Resource, "/")
	if parts[0] != "role" || parsed.Service != iam.ServiceName {
		return trace.BadParameter("%q is not an AWS IAM role ARN", parsed)
	}
	if len(parts) < 2 || len(parts[len(parts)-1]) == 0 {
		return trace.BadParameter("%q is missing AWS IAM role name", parsed)
	}
	if err := apiawsutils.IsValidAccountID(parsed.AccountID); err != nil {
		return trace.BadParameter("%q invalid account ID: %v", parsed, err)
	}
	return nil
}

// CheckARNPartitionAndAccount checks an AWS ARN against an expected AWS partition and account ID.
// An empty expected AWS partition or account ID is not checked.
func CheckARNPartitionAndAccount(ARN *arn.ARN, wantPartition, wantAccountID string) error {
	if ARN.Partition != wantPartition && wantPartition != "" {
		return trace.BadParameter("expected AWS partition %q but got %q", wantPartition, ARN.Partition)
	}
	if ARN.AccountID != wantAccountID && wantAccountID != "" {
		return trace.BadParameter("expected AWS account ID %q but got %q", wantAccountID, ARN.AccountID)
	}
	return nil
}

// IsRoleARN returns true if the provided string is a AWS role ARN.
func IsRoleARN(roleARN string) bool {
	if _, err := ParseRoleARN(roleARN); err == nil {
		return true
	}

	return IsPartialRoleARN(roleARN)
}

// IsPartialRoleARN returns true if the provided role ARN only contains the
// resource name.
func IsPartialRoleARN(roleARN string) bool {
	return strings.HasPrefix(roleARN, "role/")
}

// IsUserARN returns true if the provided string is a AWS user ARN.
func IsUserARN(userARN string) bool {
	resourceName := userARN
	if parsed, err := arn.Parse(userARN); err == nil {
		resourceName = parsed.Resource
	}

	return strings.HasPrefix(resourceName, "user/")
}
