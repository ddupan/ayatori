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

package postgresql

import (
	"context"

	"git.ddupan.top/panxiao81/ayatori/internal/database/application"
)

// 使用 pg_catalog 限定名称，避免管理账号的 search_path 改变查询来源。
// 一个语句读取版本和可用列表；ARRAY 子查询在无行时返回空数组，而非 NULL。
// 不查询 pg_extension：已安装集合不能代表服务器提供的全部扩展。
const inspectMetadataStatement = `
SELECT
    pg_catalog.current_setting('server_version'),
    ARRAY(
        SELECT name::text
        FROM pg_catalog.pg_available_extensions
        ORDER BY name
    )`

func (d *connection) InspectMetadata(ctx context.Context) (application.DatabaseMetadata, error) {
	var metadata application.DatabaseMetadata
	err := d.pool.QueryRow(ctx, inspectMetadataStatement).Scan(
		&metadata.Version,
		&metadata.AvailableExtensions,
	)
	if err != nil {
		// 不返回部分结果，也不把查询失败转换为“成功观察到空列表”。
		return application.DatabaseMetadata{}, safeError(err, application.ErrObservation)
	}
	return metadata, nil
}
