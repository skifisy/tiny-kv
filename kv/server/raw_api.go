package server

import (
	"context"

	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
)

// The functions below are Server's Raw API. (implements TinyKvServer).
// Some helper methods can be found in sever.go in the current directory

// RawGet return the corresponding Get response based on RawGetRequest's CF and Key fields
func (server *Server) RawGet(ctx context.Context, req *kvrpcpb.RawGetRequest) (*kvrpcpb.RawGetResponse, error) {
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return nil, err
	}
	// 作用：确保在函数结束时，无论是正常返回还是发生错误，reader.Close()都会被调用，以释放资源。
	defer reader.Close()  

	value, err := reader.GetCF(req.Cf, req.Key)
	if err != nil {
		return nil, err
	}

	resp := &kvrpcpb.RawGetResponse{}
	if value == nil {
		resp.NotFound = true
		return resp, nil
	}

	resp.Value = value
	_ = ctx
	return resp, nil
}

// RawPut puts the target data into storage and returns the corresponding response
func (server *Server) RawPut(_ context.Context, req *kvrpcpb.RawPutRequest) (*kvrpcpb.RawPutResponse, error) {
	modify := storage.Modify{
		Data: storage.Put{
			Key:   req.Key,
			Value: req.Value,
			Cf:    req.Cf,
		},
	}
	if err := server.storage.Write(req.Context, []storage.Modify{modify}); err != nil {
		return nil, err
	}

	return &kvrpcpb.RawPutResponse{}, nil
}

// RawDelete delete the target data from storage and returns the corresponding response
func (server *Server) RawDelete(_ context.Context, req *kvrpcpb.RawDeleteRequest) (*kvrpcpb.RawDeleteResponse, error) {
	modify := storage.Modify{
		Data: storage.Delete{
			Key: req.Key,
			Cf:  req.Cf,
		},
	}
	if err := server.storage.Write(req.Context, []storage.Modify{modify}); err != nil {
		return nil, err
	}

	return &kvrpcpb.RawDeleteResponse{}, nil
}

// RawScan scan the data starting from the start key up to limit. and return the corresponding result
func (server *Server) RawScan(_ context.Context, req *kvrpcpb.RawScanRequest) (*kvrpcpb.RawScanResponse, error) {
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	iter := reader.IterCF(req.Cf)
	defer iter.Close()

	resp := &kvrpcpb.RawScanResponse{
		Kvs: make([]*kvrpcpb.KvPair, 0, req.Limit),
	}

	for iter.Seek(req.StartKey); iter.Valid() && uint32(len(resp.Kvs)) < req.Limit; iter.Next() {
		item := iter.Item()
		value, err := item.Value()
		if err != nil {
			return nil, err
		}
		resp.Kvs = append(resp.Kvs, &kvrpcpb.KvPair{Key: item.KeyCopy(nil), Value: value})
	}

	return resp, nil
}
