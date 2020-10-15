package meta

import (
	. "github.com/journeymidnight/yig/error"
	"github.com/journeymidnight/yig/helper"
	. "github.com/journeymidnight/yig/meta/types"
	"github.com/journeymidnight/yig/redis"
)

// Note the usage info got from this method is possibly not accurate because we don't
// invalid cache when updating usage. For accurate usage info, use `GetUsage()`
func (m *Meta) GetBucket(bucketName string, willNeed bool) (bucket *Bucket, err error) {
	getBucket := func() (b interface{}, err error) {
		b, err = m.Client.GetBucket(bucketName)
		return b, err
	}
	unmarshaller := func(in []byte) (interface{}, error) {
		var bucket Bucket
		err := helper.MsgPackUnMarshal(in, &bucket)
		return &bucket, NewError(InMetaWarn, "Bucket unmarshal err", err)
	}
	b, err := m.Cache.Get(redis.BucketTable, bucketName, getBucket, unmarshaller, willNeed)
	if err != nil {
		return
	}
	bucket, ok := b.(*Bucket)
	if !ok {
		err = NewError(InMetaFatalError, "Cast bucket failed", nil)
		helper.Logger.Info(err.Error(), b)
		return
	}
	return bucket, nil
}

func (m *Meta) GetBuckets() (buckets []Bucket, err error) {
	buckets, err = m.Client.GetBuckets()
	return
}

func (m *Meta) GetUsage(bucketName string) (int64, error) {
	m.Cache.Remove(redis.BucketTable, bucketName)
	bucket, err := m.GetBucket(bucketName, true)
	if err != nil {
		return 0, err
	}
	return bucket.Usage, nil
}

func (m *Meta) GetBucketInfo(bucketName string) (*Bucket, error) {
	m.Cache.Remove(redis.BucketTable, bucketName)
	bucket, err := m.GetBucket(bucketName, true)
	if err != nil {
		return bucket, err
	}
	return bucket, nil
}

func (m *Meta) GetUserInfo(userId string) ([]string, error) {
	m.Cache.Remove(redis.UserTable, userId)
	buckets, err := m.GetUserBuckets(userId, true)
	if err != nil {
		return nil, err
	}
	return buckets, nil
}
