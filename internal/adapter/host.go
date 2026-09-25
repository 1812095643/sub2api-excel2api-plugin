package adapter

import (
	"context"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
)

// 原插件只通过同一个宿主授予的能力访问账号和 KV，转接层不扩大权限范围。
type hostRelay struct {
	pluginv1.UnimplementedHostServiceServer
	api pluginv1.HostServiceClient
}

func (r *hostRelay) KVGet(ctx context.Context, req *pluginv1.KVGetRequest) (*pluginv1.KVGetResponse, error) {
	return r.api.KVGet(ctx, req)
}
func (r *hostRelay) KVSet(ctx context.Context, req *pluginv1.KVSetRequest) (*pluginv1.KVSetResponse, error) {
	return r.api.KVSet(ctx, req)
}
func (r *hostRelay) KVDelete(ctx context.Context, req *pluginv1.KVDeleteRequest) (*pluginv1.KVDeleteResponse, error) {
	return r.api.KVDelete(ctx, req)
}
func (r *hostRelay) KVList(ctx context.Context, req *pluginv1.KVListRequest) (*pluginv1.KVListResponse, error) {
	return r.api.KVList(ctx, req)
}
func (r *hostRelay) ListAccounts(ctx context.Context, req *pluginv1.ListAccountsRequest) (*pluginv1.ListAccountsResponse, error) {
	return r.api.ListAccounts(ctx, req)
}
func (r *hostRelay) ResolveOutboundIdentity(ctx context.Context, req *pluginv1.ResolveOutboundIdentityRequest) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	return r.api.ResolveOutboundIdentity(ctx, req)
}
