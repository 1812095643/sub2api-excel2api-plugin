package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	hclog "github.com/hashicorp/go-hclog"
	hcplugin "github.com/hashicorp/go-plugin"
)

const OriginalHash = "61ebe7d84f1f710e215f827eaddfc18c1058ade0715b92694e741f9284f277de"

type Child struct {
	Client *hcplugin.Client
	API    pluginv1.TransportPluginClient
	Broker *hcplugin.GRPCBroker
}

func StartChild(ctx context.Context, path string) (*Child, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("原始传输程序不存在或不是普通文件")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("无法读取原始传输程序")
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	_ = file.Close()
	if err != nil || hex.EncodeToString(hash.Sum(nil)) != OriginalHash {
		return nil, errors.New("原始传输程序的 SHA-256 不匹配")
	}
	// 宿主只给清单入口加执行权限，嵌套程序验哈希后由当前服务用户赋予执行权限。
	if err := os.Chmod(path, 0o700); err != nil {
		return nil, errors.New("无法设置原始传输程序执行权限")
	}
	command := exec.Command(path)
	configureChildCommand(command)
	checksum, _ := hex.DecodeString(OriginalHash)
	client := hcplugin.NewClient(&hcplugin.ClientConfig{
		HandshakeConfig: pluginv1.HandshakeConfig, Plugins: pluginv1.ClientPluginMap(), Cmd: command,
		AllowedProtocols: []hcplugin.Protocol{hcplugin.ProtocolGRPC}, StartTimeout: 10 * time.Second,
		SecureConfig: &hcplugin.SecureConfig{Checksum: checksum, Hash: sha256.New()},
		Logger:       hclog.NewNullLogger(), SyncStdout: io.Discard, SyncStderr: io.Discard, SkipHostEnv: true,
	})
	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, errors.New("原始传输进程启动失败")
	}
	dispensed, err := rpcClient.Dispense(pluginv1.TransportPluginName)
	if err != nil {
		client.Kill()
		return nil, errors.New("原始传输协议握手失败")
	}
	transport, ok := dispensed.(*pluginv1.TransportClient)
	if !ok || transport.TransportPluginClient == nil {
		client.Kill()
		return nil, errors.New("原始传输接口不匹配")
	}
	infoCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	identity, err := transport.GetInfo(infoCtx, &pluginv1.GetInfoRequest{})
	if err != nil || identity.PluginId != "local.sub2api.openai-transport" || identity.PluginVersion != "0.2.7" || identity.ProtocolVersion != 1 || identity.TransportApiVersion != 1 {
		client.Kill()
		return nil, errors.New("原始传输程序身份不匹配")
	}
	return &Child{Client: client, API: transport.TransportPluginClient, Broker: transport.Broker}, nil
}

func StartBundledChild(ctx context.Context) (*Child, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return StartChild(ctx, filepath.Join(filepath.Dir(executable), "openai-transport-original"))
}

func (c *Child) Close() { c.Client.Kill() }
