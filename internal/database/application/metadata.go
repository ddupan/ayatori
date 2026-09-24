/*
Copyright 2026.

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

package application

import "git.ddupan.top/panxiao81/ayatori/internal/database/domain/instance"

// DatabaseMetadata 是一次只读查询的事实，不包含管理权限或完整就绪结论。
// AvailableExtensions 是服务器提供的可用列表，不是已安装列表或安装授权。
type DatabaseMetadata struct {
	Version             string
	AvailableExtensions []string
}

// MetadataObservation 只在查询成功且有效凭据再次核对一致后产生。
// target 绑定本次调用，而非连接最初创建时的 generation；零值表示没有观察。
type MetadataObservation struct {
	target     instance.ObservationTarget
	version    string
	extensions instance.ExtensionSupport
}

func (o MetadataObservation) Target() instance.ObservationTarget { return o.target }
func (o MetadataObservation) Version() string                    { return o.version }
func (o MetadataObservation) Extensions() instance.ExtensionSupport {
	return o.extensions
}
