package api

import (
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/journeymidnight/yig/api/datatype/policy"
	"github.com/journeymidnight/yig/context"
	. "github.com/journeymidnight/yig/error"
	"github.com/journeymidnight/yig/helper"
	"github.com/journeymidnight/yig/iam/common"
	"github.com/journeymidnight/yig/meta/types"
	"github.com/journeymidnight/yig/signature"
)

// Check request auth type verifies the incoming http request
// - validates the request signature
// - validates the policy action if anonymous tests bucket policies if any,
//   for authenticated requests validates IAM policies.
// returns APIErrorCode if any to be replied to the client.
func checkRequestAuth(r *http.Request, action policy.Action) (c common.Credential, err error) {
	// TODO:Location constraint
	ctx := context.GetRequestContext(r)
	logger := ctx.Logger
	authType := ctx.AuthType
	switch authType {
	case signature.AuthTypeUnknown:
		logger.Info("ErrAccessDenied: AuthTypeUnknown")
		return c, ErrSignatureVersionNotSupported
	case signature.AuthTypeSignedV4, signature.AuthTypePresignedV4,
		signature.AuthTypePresignedV2, signature.AuthTypeSignedV2:
		logger.Info("AuthTypeSigned:", authType)
		if c, err := signature.IsReqAuthenticated(r); err != nil {
			return c, err
		} else {
			logger.Info("Credential:", c)
			// check bucket policy
			if action == policy.ListAllMyBucketsAction || action == policy.CreateBucketAction {
				isAllow, err := IsRamPolicyAllowed(c.Policy, r, action)
				c.AllowOtherUserAccess = isAllow
				helper.Logger.Info("checkRequestAuth0:", isAllow, err)
				return c, err
			} else {
				// do bucket policy check first
				isAllow, err := IsBucketPolicyAllowed(&c, ctx.BucketInfo, r, action, ctx.ObjectName)
				helper.Logger.Info("checkRequestAuth1:", isAllow, err)
				if err == nil && isAllow == false {
					//then do ram policy check if the request is from a sub user of who own this bucket
					if c.ExternRootId == ctx.BucketInfo.OwnerId {
						isAllow, err = IsRamPolicyAllowed(c.Policy, r, action)
						helper.Logger.Info("checkRequestAuth2:", isAllow, err)
					}
				}
				c.AllowOtherUserAccess = isAllow
				return c, err
			}
		}
	case signature.AuthTypeAnonymous:
		if action == policy.ListAllMyBucketsAction {
			return c, ErrAccessDenied
		}
		isAllow, err := IsBucketPolicyAllowed(&c, ctx.BucketInfo, r, action, ctx.ObjectName)
		c.AllowOtherUserAccess = isAllow
		return c, err
	}
	return c, ErrAccessDenied
}

func checkSourceBucketAuth(r *http.Request, action policy.Action, bucket *types.Bucket, object string, c *common.Credential) (err error) {
	isAllow, err := IsBucketPolicyAllowed(c, bucket, r, action, object)
	helper.Logger.Debug("checkRequestAuth2:", isAllow, err)
	if err == nil && isAllow == false {
		//then do ram policy check if the request is from a sub user of who own this bucket
		if c.ExternRootId == bucket.OwnerId {
			isAllow, err = IsRamPolicyAllowed(c.Policy, r, action)
			helper.Logger.Debug("checkRequestAuth3:", isAllow, err)
		}
	}
	c.AllowOtherUserAccess = isAllow
	return
}

func IsBucketPolicyAllowed(c *common.Credential, bucket *types.Bucket, r *http.Request, action policy.Action, objectName string) (allow bool, err error) {
	if bucket == nil {
		return false, ErrNoSuchBucket
	}

	//the root user always hava full_control of his bucket
	if bucket.OwnerId == c.ExternRootId && c.ExternUserId == c.ExternRootId {
		return true, nil
	}

	var p policy.Policy
	err = json.Unmarshal(bucket.Policy, &p)
	if err != nil {
		return false, NewError(InDatatypeFatalError, "IsBucketPolicyAllowed unmarshal err", err)
	}
	policyResult := p.IsAllowed(policy.Args{
		// TODO: Add IAM policy. Current account name is always useless.
		AccountName:     c.ExternUserId,
		Action:          action,
		BucketName:      bucket.Name,
		ConditionValues: getConditionValues(r, ""),
		IsOwner:         false,
		ObjectName:      objectName,
	})
	if policyResult == policy.PolicyAllow {
		return true, nil
	} else if policyResult == policy.PolicyDeny {
		return false, ErrAccessDenied
	} else {
		return false, nil
	}

}

func IsRamPolicyAllowed(p *policy.Policy, r *http.Request, action policy.Action) (allow bool, err error) {
	//just care about action
	if p == nil {
		if action == policy.ListAllMyBucketsAction || action == policy.CreateBucketAction {
			return false, ErrAccessDenied
		} else {
			return false, nil
		}
	}

	policyResult := p.IsAllowed(policy.Args{
		AccountName:     "",
		Action:          action,
		BucketName:      "",
		ConditionValues: getConditionValues(r, ""),
		IsOwner:         false,
		ObjectName:      "",
	})
	if policyResult == policy.PolicyAllow {
		return true, nil
	} else if policyResult == policy.PolicyDeny {
		return false, ErrAccessDenied
	} else {
		if action == policy.ListAllMyBucketsAction || action == policy.CreateBucketAction {
			return false, ErrAccessDenied
		} else {
			return false, nil
		}
	}
}

func getConditionValues(request *http.Request, locationConstraint string) map[string][]string {
	args := make(map[string][]string)

	for key, values := range request.Header {
		if existingValues, found := args[key]; found {
			args[key] = append(existingValues, values...)
		} else {
			args[key] = values
		}
	}

	for key, values := range request.URL.Query() {
		if existingValues, found := args[key]; found {
			args[key] = append(existingValues, values...)
		} else {
			args[key] = values
		}
	}

	args["SourceIp"] = []string{GetSourceIP(request)}

	if locationConstraint != "" {
		args["LocationConstraint"] = []string{locationConstraint}
	}

	return args
}

var (
	// De-facto standard header keys.
	xForwardedFor = http.CanonicalHeaderKey("X-Forwarded-For")
	xRealIP       = http.CanonicalHeaderKey("X-Real-IP")

	// RFC7239 defines a new "Forwarded: " header designed to replace the
	// existing use of X-Forwarded-* headers.
	// e.g. Forwarded: for=192.0.2.60;proto=https;by=203.0.113.43
	forwarded = http.CanonicalHeaderKey("Forwarded")
	// Allows for a sub-match of the first value after 'for=' to the next
	// comma, semi-colon or space. The match is case-insensitive.
	forRegex = regexp.MustCompile(`(?i)(?:for=)([^(;|,| )]+)(.*)`)
	// Allows for a sub-match for the first instance of scheme (http|https)
	// prefixed by 'proto='. The match is case-insensitive.

)

// GetSourceIP retrieves the IP from the X-Forwarded-For, X-Real-IP and RFC7239
// Forwarded headers (in that order), falls back to r.RemoteAddr when all
// else fails.
func GetSourceIP(r *http.Request) string {
	var addr string

	if fwd := r.Header.Get(xForwardedFor); fwd != "" {
		// Only grab the first (client) address. Note that '192.168.0.1,
		// 10.1.1.1' is a valid key for X-Forwarded-For where addresses after
		// the first may represent forwarding proxies earlier in the chain.
		s := strings.Index(fwd, ", ")
		if s == -1 {
			s = len(fwd)
		}
		addr = fwd[:s]
	} else if fwd := r.Header.Get(xRealIP); fwd != "" {
		// X-Real-IP should only contain one IP address (the client making the
		// request).
		addr = fwd
	} else if fwd := r.Header.Get(forwarded); fwd != "" {
		// match should contain at least two elements if the protocol was
		// specified in the Forwarded header. The first element will always be
		// the 'for=' capture, which we ignore. In the case of multiple IP
		// addresses (for=8.8.8.8, 8.8.4.4, 172.16.1.20 is valid) we only
		// extract the first, which should be the client IP.
		if match := forRegex.FindStringSubmatch(fwd); len(match) > 1 {
			// IPv6 addresses in Forwarded headers are quoted-strings. We strip
			// these quotes.
			addr = strings.Trim(match[1], `"`)
		}
	}

	if addr != "" {
		return addr
	}

	// Default to remote address if headers not set.
	addr, _, _ = net.SplitHostPort(r.RemoteAddr)
	return addr
}
