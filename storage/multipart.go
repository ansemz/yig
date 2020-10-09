package storage

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/journeymidnight/yig/api/datatype"
	. "github.com/journeymidnight/yig/context"
	"github.com/journeymidnight/yig/crypto"
	. "github.com/journeymidnight/yig/error"
	"github.com/journeymidnight/yig/helper"
	"github.com/journeymidnight/yig/iam"
	"github.com/journeymidnight/yig/iam/common"
	. "github.com/journeymidnight/yig/meta/common"
	meta "github.com/journeymidnight/yig/meta/types"
	"github.com/journeymidnight/yig/redis"
	"github.com/journeymidnight/yig/signature"
)

// http://docs.aws.amazon.com/AmazonS3/latest/dev/UploadingObjects.html
const (
	// minimum Part size for multipart upload is 100Kb
	MIN_PART_SIZE = 100 << 10 // 100Kb
	// maximum Part size per PUT request is 5GiB
	MAX_PART_SIZE = 5 << 30 // 5GB
	// maximum Part number for multipart upload is 10000 (Acceptable values range from 1 to 10000 inclusive)
	MAX_PART_NUMBER = 10000
)

func (yig *YigStorage) ListMultipartUploads(reqCtx RequestContext, credential common.Credential,
	request datatype.ListUploadsRequest) (result datatype.ListMultipartUploadsResponse, err error) {

	bucket := reqCtx.BucketInfo
	if bucket == nil {
		return result, ErrNoSuchBucket
	}

	if !credential.AllowOtherUserAccess {
		//an CanonicalUser request
		if !(bucket.OwnerId == credential.ExternRootId && credential.ExternUserId == credential.ExternRootId) {
			if bucket.ACL.CannedAcl != "" {
				switch bucket.ACL.CannedAcl {
				case "public-read", "public-read-write":
					break
				case "authenticated-read":
					if credential.ExternUserId == "" {
						err = ErrBucketAccessForbidden
						return
					}
				default:
					err = ErrBucketAccessForbidden
					return
				}
			} else {
				switch true {
				case datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_READ, credential.ExternUserId) ||
					datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, credential.ExternUserId):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_READ, datatype.ACL_GROUP_TYPE_ALL_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_ALL_USERS):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_READ, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS):
					if credential.ExternUserId != "" {
						break
					}
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_READ, datatype.ACL_GROUP_TYPE_LOG_DELIVERY) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_LOG_DELIVERY):
					if helper.StringInSlice(credential.ExternUserId, helper.CONFIG.LogDeliveryGroup) {
						break
					}
				default:
					err = ErrBucketAccessForbidden
					return
				}
			}
		}
	}

	result, err = yig.MetaStorage.Client.ListMultipartUploads(bucket.Name, request.KeyMarker, request.UploadIdMarker, request.Prefix, request.Delimiter, request.EncodingType, request.MaxUploads)
	if err != nil {
		return
	}

	result.EncodingType = request.EncodingType
	if result.EncodingType != "" { // only support "url" encoding for now
		result.Delimiter = url.QueryEscape(result.Delimiter)
		result.KeyMarker = url.QueryEscape(result.KeyMarker)
		result.Prefix = url.QueryEscape(result.Prefix)
		result.NextKeyMarker = url.QueryEscape(result.NextKeyMarker)
	}
	return
}

func (yig *YigStorage) NewMultipartUpload(reqCtx RequestContext, credential common.Credential,
	metadata map[string]string, acl datatype.Acl,
	sseRequest datatype.SseRequest, storageClass StorageClass) (uploadId string, err error) {
	bucketName, objectName := reqCtx.BucketName, reqCtx.ObjectName
	bucket := reqCtx.BucketInfo
	if bucket == nil {
		return "", ErrNoSuchBucket
	}

	if !credential.AllowOtherUserAccess {
		//an CanonicalUser request
		if !(bucket.OwnerId == credential.ExternRootId && credential.ExternUserId == credential.ExternRootId) {
			if bucket.ACL.CannedAcl != "" {
				switch bucket.ACL.CannedAcl {
				case "public-read-write":
					break
				default:
					err = ErrBucketAccessForbidden
					return
				}
			} else {
				switch true {
				case datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, credential.ExternUserId) ||
					datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, credential.ExternUserId):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_ALL_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_ALL_USERS):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS):
					if credential.ExternUserId != "" {
						break
					}
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_LOG_DELIVERY) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_LOG_DELIVERY):
					if helper.StringInSlice(credential.ExternUserId, helper.CONFIG.LogDeliveryGroup) {
						break
					}
				default:
					err = ErrBucketAccessForbidden
					return
				}
			}
		}
	}

	if bucket.Versioning == datatype.BucketVersioningDisabled {
		if reqCtx.IsObjectForbidOverwrite {
			if reqCtx.ObjectInfo != nil {
				return "", ErrForbiddenOverwriteKey
			}
		}
	}
	contentType, ok := metadata["Content-Type"]
	if !ok {
		contentType = "application/octet-stream"
	}

	cephCluster, pool := yig.pickClusterAndPool(bucketName, objectName, storageClass, -1, false)
	multipartMetadata := meta.MultipartMetadata{
		InitiatorId:  bucket.OwnerId,
		OwnerId:      bucket.OwnerId,
		ContentType:  contentType,
		Location:     cephCluster.ID(),
		Pool:         pool,
		Acl:          acl,
		SseRequest:   sseRequest,
		Attrs:        metadata,
		StorageClass: storageClass,
	}
	if sseRequest.Type == crypto.S3.String() {
		multipartMetadata.EncryptionKey, multipartMetadata.CipherKey, err = yig.encryptionKeyFromSseRequest(sseRequest, bucketName, objectName)
		if err != nil {
			return
		}
	} else {
		multipartMetadata.EncryptionKey = nil
	}

	multipart := meta.Multipart{
		BucketName:  bucketName,
		ObjectName:  objectName,
		InitialTime: uint64(time.Now().UTC().UnixNano()),
		Metadata:    multipartMetadata,
	}

	err = multipart.GenUploadId()
	if err != nil {
		return
	}
	err = yig.MetaStorage.Client.CreateMultipart(multipart)
	if err != nil {
		return
	}
	return multipart.UploadId, nil
}

func (yig *YigStorage) PutObjectPart(reqCtx RequestContext, credential common.Credential,
	uploadId string, partId int, size int64, data io.ReadCloser, md5Hex string,
	sseRequest datatype.SseRequest) (result datatype.PutObjectPartResult, err error) {
	defer data.Close()

	bucket := reqCtx.BucketInfo
	if bucket == nil {
		err = ErrNoSuchBucket
		return
	}

	bucketName, objectName := reqCtx.BucketName, reqCtx.ObjectName
	multipart, err := yig.MetaStorage.GetMultipart(bucketName, objectName, uploadId)
	if err != nil {
		return
	}

	if size > MAX_PART_SIZE {
		err = ErrEntityTooLarge
		return
	}

	if !credential.AllowOtherUserAccess {
		//an CanonicalUser request
		if !(bucket.OwnerId == credential.ExternRootId && credential.ExternUserId == credential.ExternRootId) {
			if bucket.ACL.CannedAcl != "" {
				switch bucket.ACL.CannedAcl {
				case "public-read-write":
					break
				default:
					err = ErrBucketAccessForbidden
					return
				}
			} else {
				switch true {
				case datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, credential.ExternUserId) ||
					datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, credential.ExternUserId):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_ALL_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_ALL_USERS):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS):
					if credential.ExternUserId != "" {
						break
					}
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_LOG_DELIVERY) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_LOG_DELIVERY):
					if helper.StringInSlice(credential.ExternUserId, helper.CONFIG.LogDeliveryGroup) {
						break
					}
				default:
					err = ErrBucketAccessForbidden
					return
				}
			}
		}
	}

	var encryptionKey []byte
	switch multipart.Metadata.SseRequest.Type {
	case "":
		break
	case crypto.SSEC.String():
		if sseRequest.Type != crypto.SSEC.String() {
			err = ErrInvalidSseHeader
			return
		}
		encryptionKey = sseRequest.SseCustomerKey
	case crypto.S3.String():
		encryptionKey = multipart.Metadata.EncryptionKey
	case crypto.S3KMS.String():
		err = ErrNotImplemented
		return
	}

	md5Writer := md5.New()
	limitedDataReader := io.LimitReader(data, size)
	poolName := multipart.Metadata.Pool
	cluster, err := yig.GetClusterByFsName(multipart.Metadata.Location)
	if err != nil {
		return
	}
	dataReader := io.TeeReader(limitedDataReader, md5Writer)

	var initializationVector []byte
	if len(encryptionKey) != 0 {
		initializationVector, err = newInitializationVector()
		if err != nil {
			return
		}
	}
	storageReader, err := wrapEncryptionReader(dataReader, encryptionKey,
		initializationVector)
	if err != nil {
		return
	}

	throttleReader := yig.MetaStorage.QosMeta.NewThrottleReader(bucketName, storageReader)
	defer throttleReader.Close()
	objectId, bytesWritten, err := cluster.Put(poolName, throttleReader)
	if err != nil {
		return
	}
	// Should metadata update failed, add `maybeObjectToRecycle` to `RecycleQueue`,
	// so the object in Ceph could be removed asynchronously
	maybeObjectToRecycle := objectToRecycle{
		location:   cluster.ID(),
		pool:       poolName,
		objectId:   objectId,
		objectType: meta.ObjectTypeMultipart,
	}
	if int64(bytesWritten) < size {
		RecycleQueue <- maybeObjectToRecycle
		err = ErrIncompleteBody
		return
	}

	calculatedMd5 := hex.EncodeToString(md5Writer.Sum(nil))
	if md5Hex != "" && md5Hex != calculatedMd5 {
		RecycleQueue <- maybeObjectToRecycle
		err = ErrBadDigest
		return
	}

	if signVerifyReader, ok := data.(*signature.SignVerifyReadCloser); ok {
		credential, err = signVerifyReader.Verify()
		if err != nil {
			RecycleQueue <- maybeObjectToRecycle
			return
		}
	}

	part := meta.Part{
		PartNumber:           partId,
		Size:                 size,
		ObjectId:             objectId,
		Etag:                 calculatedMd5,
		LastModified:         time.Now().UTC().Format(meta.CREATE_TIME_LAYOUT),
		InitializationVector: initializationVector,
	}
	deltaSize, err := yig.MetaStorage.PutObjectPart(multipart, part)
	if err != nil {
		RecycleQueue <- maybeObjectToRecycle
		return result, err
	}
	// remove possible old object in Ceph
	if part, ok := multipart.Parts[partId]; ok {
		RecycleQueue <- objectToRecycle{
			location:   multipart.Metadata.Location,
			pool:       multipart.Metadata.Pool,
			objectType: meta.ObjectTypeMultipart,
			objectId:   part.ObjectId,
		}
	}

	result.ETag = calculatedMd5
	result.SseType = sseRequest.Type
	result.SseAwsKmsKeyIdBase64 = base64.StdEncoding.EncodeToString([]byte(sseRequest.SseAwsKmsKeyId))
	result.SseCustomerAlgorithm = sseRequest.SseCustomerAlgorithm
	result.SseCustomerKeyMd5Base64 = base64.StdEncoding.EncodeToString(sseRequest.SseCustomerKey)
	result.DeltaSize = datatype.DeltaSizeInfo{StorageClass: multipart.Metadata.StorageClass, Delta: deltaSize}
	return result, nil
}

func (yig *YigStorage) CopyObjectPart(bucketName, objectName, uploadId string, partId int,
	size int64, data io.Reader, credential common.Credential,
	sseRequest datatype.SseRequest) (result datatype.PutObjectPartResult, err error) {

	multipart, err := yig.MetaStorage.GetMultipart(bucketName, objectName, uploadId)
	if err != nil {
		return
	}

	if size > MAX_PART_SIZE {
		err = ErrEntityTooLarge
		return
	}

	var encryptionKey []byte
	switch multipart.Metadata.SseRequest.Type {
	case "":
		break
	case crypto.SSEC.String():
		if sseRequest.Type != crypto.SSEC.String() {
			err = ErrInvalidSseHeader
			return
		}
		encryptionKey = sseRequest.SseCustomerKey
	case crypto.S3.String():
		encryptionKey = multipart.Metadata.EncryptionKey
	case crypto.S3KMS.String():
		err = ErrNotImplemented
		return
	}

	md5Writer := md5.New()
	limitedDataReader := io.LimitReader(data, size)
	poolName := multipart.Metadata.Pool
	cephCluster, err := yig.GetClusterByFsName(multipart.Metadata.Location)
	if err != nil {
		return
	}
	dataReader := io.TeeReader(limitedDataReader, md5Writer)

	var initializationVector []byte
	if len(encryptionKey) != 0 {
		initializationVector, err = newInitializationVector()
		if err != nil {
			return
		}
	}
	storageReader, err := wrapEncryptionReader(dataReader, encryptionKey,
		initializationVector)
	if err != nil {
		return
	}

	throttleReader := yig.MetaStorage.QosMeta.NewThrottleReader(bucketName, storageReader)
	defer throttleReader.Close()
	objectId, bytesWritten, err := cephCluster.Put(poolName, throttleReader)
	if err != nil {
		return
	}
	// Should metadata update failed, add `maybeObjectToRecycle` to `RecycleQueue`,
	// so the object in Ceph could be removed asynchronously
	maybeObjectToRecycle := objectToRecycle{
		location:   cephCluster.ID(),
		pool:       poolName,
		objectId:   objectId,
		objectType: meta.ObjectTypeMultipart,
	}

	if int64(bytesWritten) < size {
		RecycleQueue <- maybeObjectToRecycle
		err = ErrIncompleteBody
		return
	}

	result.ETag = hex.EncodeToString(md5Writer.Sum(nil))

	bucket, err := yig.MetaStorage.GetBucket(bucketName, true)
	if err != nil {
		RecycleQueue <- maybeObjectToRecycle
		return
	}

	if !credential.AllowOtherUserAccess {
		//an CanonicalUser request
		if !(bucket.OwnerId == credential.ExternRootId && credential.ExternUserId == credential.ExternRootId) {
			if bucket.ACL.CannedAcl != "" {
				switch bucket.ACL.CannedAcl {
				case "public-read-write":
					break
				default:
					RecycleQueue <- maybeObjectToRecycle
					err = ErrBucketAccessForbidden
					return
				}
			} else {
				switch true {
				case datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, credential.ExternUserId) ||
					datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, credential.ExternUserId):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_ALL_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_ALL_USERS):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS):
					if credential.ExternUserId != "" {
						break
					}
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_LOG_DELIVERY) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_LOG_DELIVERY):
					if helper.StringInSlice(credential.ExternUserId, helper.CONFIG.LogDeliveryGroup) {
						break
					}
				default:
					RecycleQueue <- maybeObjectToRecycle
					err = ErrBucketAccessForbidden
					return
				}
			}
		}
	}

	if initializationVector == nil {
		initializationVector = []byte{}
	}
	now := time.Now().UTC()
	part := meta.Part{
		PartNumber:           partId,
		Size:                 size,
		ObjectId:             objectId,
		Etag:                 result.ETag,
		LastModified:         now.Format(meta.CREATE_TIME_LAYOUT),
		InitializationVector: initializationVector,
	}
	result.LastModified = now

	deltaSize, err := yig.MetaStorage.PutObjectPart(multipart, part)
	if err != nil {
		RecycleQueue <- maybeObjectToRecycle
		return result, err
	}

	// remove possible old object in Ceph
	if part, ok := multipart.Parts[partId]; ok {
		RecycleQueue <- objectToRecycle{
			location: multipart.Metadata.Location,
			pool:     multipart.Metadata.Pool,
			objectId: part.ObjectId,
		}
	}
	result.DeltaSize = datatype.DeltaSizeInfo{StorageClass: multipart.Metadata.StorageClass, Delta: deltaSize}
	return result, nil
}

func (yig *YigStorage) ListObjectParts(credential common.Credential, bucketName, objectName string,
	request datatype.ListPartsRequest) (result datatype.ListPartsResponse, err error) {

	multipart, err := yig.MetaStorage.GetMultipart(bucketName, objectName, request.UploadId)
	if err != nil {
		return
	}

	initiatorId := multipart.Metadata.InitiatorId
	ownerId := multipart.Metadata.OwnerId

	bucket, err := yig.MetaStorage.GetBucket(bucketName, true)
	if err != nil {
		return
	}

	if !credential.AllowOtherUserAccess {
		//an CanonicalUser request
		if !(bucket.OwnerId == credential.ExternRootId && credential.ExternUserId == credential.ExternRootId) {
			if bucket.ACL.CannedAcl != "" {
				switch bucket.ACL.CannedAcl {
				case "public-read", "public-read-write":
					break
				case "authenticated-read":
					if credential.ExternUserId == "" {
						err = ErrBucketAccessForbidden
						return
					}
				default:
					if ownerId != credential.ExternUserId {
						err = ErrAccessDenied
						return
					}
				}
			} else {
				switch true {
				case datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_READ, credential.ExternUserId) ||
					datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, credential.ExternUserId):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_READ, datatype.ACL_GROUP_TYPE_ALL_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_ALL_USERS):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_READ, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS):
					if credential.ExternUserId != "" {
						break
					}
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_READ, datatype.ACL_GROUP_TYPE_LOG_DELIVERY) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_LOG_DELIVERY):
					if helper.StringInSlice(credential.ExternUserId, helper.CONFIG.LogDeliveryGroup) {
						break
					}
				default:
					if ownerId != credential.ExternUserId {
						err = ErrAccessDenied
						return
					}
				}
			}
		}
	}

	for i := request.PartNumberMarker + 1; i <= MAX_PART_NUMBER; i++ {
		if p, ok := multipart.Parts[i]; ok {
			part := datatype.Part{
				PartNumber:   i,
				ETag:         "\"" + p.Etag + "\"",
				LastModified: p.LastModified,
				Size:         p.Size,
			}
			result.Parts = append(result.Parts, part)

			if len(result.Parts) > request.MaxParts {
				break
			}
		}
	}
	if len(result.Parts) == request.MaxParts+1 {
		result.IsTruncated = true
		result.NextPartNumberMarker = result.Parts[request.MaxParts].PartNumber
		result.Parts = result.Parts[:request.MaxParts]
	}

	var user common.Credential
	user, err = iam.GetCredentialByUserId(ownerId)
	if err != nil {
		return
	}
	result.Owner.ID = user.ExternUserId
	result.Owner.DisplayName = user.DisplayName
	user, err = iam.GetCredentialByUserId(initiatorId)
	if err != nil {
		return
	}
	result.Initiator.ID = user.ExternUserId
	result.Initiator.DisplayName = user.DisplayName

	result.Bucket = bucketName
	result.Key = objectName
	result.UploadId = request.UploadId
	result.StorageClass = multipart.Metadata.StorageClass.ToString()
	result.PartNumberMarker = request.PartNumberMarker
	result.MaxParts = request.MaxParts
	result.EncodingType = request.EncodingType

	if result.EncodingType != "" { // only support "url" encoding for now
		result.Key = url.QueryEscape(result.Key)
	}
	return
}

func (yig *YigStorage) AbortMultipartUpload(reqCtx RequestContext, credential common.Credential, uploadId string) (deltaInfo datatype.DeltaSizeInfo, err error) {
	bucket := reqCtx.BucketInfo
	if bucket == nil {
		return deltaInfo, ErrNoSuchBucket
	}

	if !credential.AllowOtherUserAccess {
		//an CanonicalUser request
		if !(bucket.OwnerId == credential.ExternRootId && credential.ExternUserId == credential.ExternRootId) {
			if bucket.ACL.CannedAcl != "" {
				switch bucket.ACL.CannedAcl {
				case "public-read-write":
					break
				default:
					err = ErrAccessDenied
					return
				}
			} else {
				switch true {
				case datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, credential.ExternUserId) ||
					datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, credential.ExternUserId):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_ALL_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_ALL_USERS):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS):
					if credential.ExternUserId != "" {
						break
					}
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_LOG_DELIVERY) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_LOG_DELIVERY):
					if helper.StringInSlice(credential.ExternUserId, helper.CONFIG.LogDeliveryGroup) {
						break
					}
				default:
					err = ErrAccessDenied
					return

				}
			}
		}
	}

	bucketName, objectName := reqCtx.BucketName, reqCtx.ObjectName
	multipart, err := yig.MetaStorage.GetMultipart(bucketName, objectName, uploadId)
	if err != nil {
		return deltaInfo, err
	}

	var removedSize int64
	removedSize, err = yig.MetaStorage.DeleteMultipart(multipart)
	if err != nil {
		return deltaInfo, err
	}
	// remove parts in Ceph

	for _, p := range multipart.Parts {
		RecycleQueue <- objectToRecycle{
			location: multipart.Metadata.Location,
			pool:     multipart.Metadata.Pool,
			objectId: p.ObjectId,
		}
	}
	deltaInfo.StorageClass = multipart.Metadata.StorageClass
	deltaInfo.Delta = -removedSize
	return deltaInfo, nil
}

func (yig *YigStorage) CompleteMultipartUpload(reqCtx RequestContext, credential common.Credential, uploadId string, uploadedParts []meta.CompletePart) (result datatype.CompleteMultipartResult,
	err error) {
	bucketName, objectName := reqCtx.BucketName, reqCtx.ObjectName
	bucket := reqCtx.BucketInfo
	if bucket == nil {
		return result, ErrNoSuchBucket
	}

	if !credential.AllowOtherUserAccess {
		//an CanonicalUser request
		if !(bucket.OwnerId == credential.ExternRootId && credential.ExternUserId == credential.ExternRootId) {
			if bucket.ACL.CannedAcl != "" {
				switch bucket.ACL.CannedAcl {
				case "public-read-write":
					break
				default:
					err = ErrAccessDenied
					return
				}
			} else {
				switch true {
				case datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, credential.ExternUserId) ||
					datatype.IsPermissionMatchedById(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, credential.ExternUserId):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_ALL_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_ALL_USERS):
					break
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_AUTHENTICATED_USERS):
					if credential.ExternUserId != "" {
						break
					}
				case datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_WRITE, datatype.ACL_GROUP_TYPE_LOG_DELIVERY) ||
					datatype.IsPermissionMatchedByGroup(bucket.ACL.Policy, datatype.ACL_PERM_FULL_CONTROL, datatype.ACL_GROUP_TYPE_LOG_DELIVERY):
					if helper.StringInSlice(credential.ExternUserId, helper.CONFIG.LogDeliveryGroup) {
						break
					}
				default:
					err = ErrAccessDenied
					return

				}
			}
		}
	}

	multipart, err := yig.MetaStorage.GetMultipart(bucketName, objectName, uploadId)
	if err != nil {
		return
	}

	md5Writer := md5.New()
	var totalSize int64 = 0
	reqCtx.Logger.Info("Upload parts:", uploadedParts, "uploadId:", uploadId)
	for i := 0; i < len(uploadedParts); i++ {
		if uploadedParts[i].PartNumber != i+1 {
			reqCtx.Logger.Error("uploadedParts[i].PartNumber != i+1; i:", i,
				"uploadId:", uploadId)
			err = ErrInvalidPart
			return
		}
		part, ok := multipart.Parts[i+1]
		if !ok {
			reqCtx.Logger.Error("multipart.Parts[i+1] does not exist; i:", i,
				"uploadId:", uploadId)
			err = ErrInvalidPart
			return
		}
		if part.Size < MIN_PART_SIZE && part.PartNumber != len(uploadedParts) {
			err = meta.PartTooSmall{
				PartSize:   part.Size,
				PartNumber: part.PartNumber,
				PartETag:   part.Etag,
			}
			return
		}
		if part.Etag != uploadedParts[i].ETag {
			reqCtx.Logger.Error("part.Etag != uploadedParts[i].ETag;",
				"i:", i, "Etag:", part.Etag, "reqEtag:",
				uploadedParts[i].ETag, "uploadId:", uploadId)
			err = ErrInvalidPart
			return
		}
		var etagBytes []byte
		etagBytes, err = hex.DecodeString(part.Etag)
		if err != nil {
			reqCtx.Logger.Error("hex.DecodeString(part.Etag) err:", err,
				"uploadId:", uploadId)
			err = ErrInvalidPart
			return
		}
		part.Offset = totalSize
		totalSize += part.Size
		md5Writer.Write(etagBytes)
	}
	result.ETag = hex.EncodeToString(md5Writer.Sum(nil))
	result.ETag += "-" + strconv.Itoa(len(uploadedParts))
	// See http://stackoverflow.com/questions/12186993
	// for how to calculate multipart Etag

	// Add to objects table
	contentType := multipart.Metadata.ContentType
	now := time.Now().UTC()
	object := &meta.Object{
		Name:             objectName,
		BucketName:       bucketName,
		OwnerId:          multipart.Metadata.OwnerId,
		Pool:             multipart.Metadata.Pool,
		Location:         multipart.Metadata.Location,
		Size:             totalSize,
		LastModifiedTime: now,
		Etag:             result.ETag,
		ContentType:      contentType,
		Parts:            multipart.Parts,
		ACL:              multipart.Metadata.Acl,
		NullVersion:      helper.Ternary(bucket.Versioning == datatype.BucketVersioningEnabled, false, true).(bool),
		DeleteMarker:     false,
		SseType:          multipart.Metadata.SseRequest.Type,
		EncryptionKey:    multipart.Metadata.CipherKey,
		CustomAttributes: multipart.Metadata.Attrs,
		Type:             meta.ObjectTypeMultipart,
		StorageClass:     multipart.Metadata.StorageClass,
		CreateTime:       uint64(now.UnixNano()),
	}
	object.VersionId = object.GenVersionId(bucket.Versioning)
	if eldObject := reqCtx.ObjectInfo; eldObject != nil {
		if eldObject.StorageClass == ObjectStorageClassGlacier && bucket.Versioning != datatype.BucketVersioningEnabled {
			freezer, err := yig.MetaStorage.GetFreezer(object.BucketName, object.Name, object.VersionId)
			if err == nil {
				err = yig.MetaStorage.DeleteFreezer(freezer)
				if err != nil {
					return result, err
				}
			} else if err != ErrNoSuchKey {
				return result, err
			}
		}
	}

	_, err = yig.MetaStorage.PutObject(reqCtx, object, &multipart, false)
	if err != nil {
		return
	}

	sseRequest := multipart.Metadata.SseRequest
	result.ObjectSize = object.Size
	result.ContentType = object.ContentType
	result.CreateTime = object.CreateTime
	result.SseType = sseRequest.Type
	result.SseAwsKmsKeyIdBase64 = base64.StdEncoding.EncodeToString([]byte(sseRequest.SseAwsKmsKeyId))
	result.SseCustomerAlgorithm = sseRequest.SseCustomerAlgorithm
	result.SseCustomerKeyMd5Base64 = base64.StdEncoding.EncodeToString(sseRequest.SseCustomerKey)

	yig.MetaStorage.Cache.Remove(redis.ObjectTable, bucketName+":"+objectName+":"+object.VersionId)
	yig.DataCache.Remove(bucketName + ":" + objectName + ":" + object.VersionId)
	if reqCtx.ObjectInfo != nil && reqCtx.BucketInfo.Versioning != datatype.BucketVersioningEnabled {
		go yig.removeOldObject(reqCtx.ObjectInfo)
	}

	return
}
