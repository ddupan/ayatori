# ADR-0026：LibvirtDomain 的 domain 改为由 libvirtxml 机械映射的结构化字段

- 状态：Proposed（需先完成下文"待验证"的事项）
- 日期：2026-11-12
- 依据：维护者讨论；spike（libvirt 12.8.0 的 `tests/qemuxmlconfdata` 语料，libvirtxml v1.12008.0）。
  修订 [ADR-0015](0015-libvirt-lxc-compute-backend.md) 的"domain：类型化设备 + 原生 XML"，以及其评估中
  对"从 libvirt schema 生成完整的 YAML API"的否决。

## 问题

ADR-0015 把 domain 按归属拆开：引用 Ayatori 对象的设备是类型化字段，其余是原生 `domainXML`，Ayatori
拥有的元素出现在 XML 中就拒绝。随着实现推进，归 Ayatori 的部分越来越多，这条规则暴露出几个问题：

- **每接管一个元素都是不兼容变更。** 控制台中继需要接管 `<serial>`（ADR-0016），VNC 只监听 unix socket
  需要接管 `<graphics>` 的监听，此外还有 qemu-guest-agent 的 `<channel>`、`<memballoon>`、`<cpu>` 兼容比较。
  元素一进入禁止清单，已经写了它的对象在 Agent 升级后就会下发失败。
- **所有权只能以整个元素为单位。** Ayatori 接管串口后，用户就不能再加自己的串口（接设备、调试）。改成
  合并也不行：用户写了 `pty` 串口，Ayatori 再加一个中继用的串口，哪个是 ttyS0 取决于 XML 的内容，从
  spec 上看不出来。
- **默认值无法落在 API 上。** 我们希望 VM 默认使用 UEFI、默认带一个由 Ayatori 中继的控制台串口。
  CRD 的默认值对 XML 字符串无能为力，只能由 Agent 在渲染时隐式添加，spec 和实际运行的 domain 不一致。
  这类默认值很重要：compute-dev1 上不带 `<video>` 的 BIOS VM 会无限重启（SeaBIOS 在没有 VGA 时
  `int 10h` 只是一个桩，Debian 的 GRUB i386-pc 随之跳进 0xa000 段触发 #UD，三重故障），UEFI 或加上
  `<video>` 都能避免。
- **校验发生在下发时。** apiserver 不知道 domain 的结构，错误只能在 Agent 下发后写进 status；CEL、
  `kubectl explain` 和结构化的 diff 都用不上。

## 评估

- **维持原生 XML，扩充禁止清单**：上述问题都不解决。
- **常用部分手写类型化字段，XML 降为只能追加的逃生口**：类型化字段会越做越像一个残缺的 domain XML
  （ADR-0015 否决强类型 VM API 的原因），类型化部分与逃生口之间的边界又是一套需要学习的规则。
- **机械映射整个 libvirtxml 模型（采用）。** ADR-0015 当时否决"从 libvirt schema 生成完整的 YAML API"
  有三个理由，spike 的结果逐条回应：
  - *CRD 过于庞大*：从 libvirtxml 的 domain 模型生成的 schema 为 212KB（513 个结构体类型、1812 个字段），
    整个 CRD 对象 210KB，apiserver 在 1 秒内接受；etcd 的单对象上限是 1.5MB。创建对象首次约 2 秒
    （CEL 与 schema 预热），之后最大的样本（30KB XML）47ms，普通 VM 7ms。
  - *要追随 libvirt 版本*：Agent 现在已经用 libvirtxml 解析 `domainXML` 再重新序列化（`xml.go`），
    libvirtxml 表达不了的内容今天就会丢失。API 的表达力本来就等于 libvirtxml 的模型，结构化只是把这条
    边界前移到 apiserver。新的 libvirt 特性需要升级 libvirtxml，与今天相同。
  - *又是一种需要学习的方言*：映射规则是机械的，不存在逐字段手写的对应关系，一一对应由检查保证（见
    下文"映射规则"）。另外提供 XML 与 YAML 的互转工具，习惯写 XML 的人照常写再转换。
- **key 照抄 XML 的名字**（`autoport`、`tickpolicy`，联合体按判别值命名）：与 libvirt 文档一致，但
  libvirtxml 用 60 个自定义 marshaler 表达联合体，判别值散落在代码里，无法从结构体机械推出，一一对应
  也就无法保证。维护者的要求是一一对应，而不是照抄 libvirt 文档，因此不采用。
- **把 513 个结构体生成为 API 包中的 Go 类型**：controller-gen 能直接生成 schema，但 API 包和 deepcopy
  会膨胀，类型与 libvirtxml 重复，消费方还要再转换一次。改为 Go 字段保存 JSON，类型就是 libvirtxml 本身
  （见下文"实现"）。

## 决策

### 形态

```yaml
spec:
  nodeName: compute-dev1
  runState: Running
  disks:                       # 只剩引用：按 alias 把 Ayatori 对象绑定到 domain 中的设备
  - alias: ua-root
    volume: dns1-root          # LibvirtStorageVolume
  - alias: ua-install
    image: debian-13-netinst   # ISO 类型的 Image
  interfaces:
  - alias: ua-net0
    network: vlan20            # LibvirtNetwork
  domain:                      # libvirtxml.Domain 的机械映射
    type: kvm
    memory: {value: 2, unit: GiB}
    vcpu: {value: 2}
    os:
      firmware: efi            # CRD 默认值
      type: {arch: x86_64, machine: q35, type: hvm}
    features:
      acpi: {}
    devices:
      disks:
      - alias: {name: ua-root}
        device: disk
        driver: {name: qemu, type: raw, discard: unmap}
        target: {dev: vda, bus: virtio}
        boot: {order: 1}
      - alias: {name: ua-install}
        device: cdrom
        target: {dev: sda, bus: sata}
        readOnly: {}
      interfaces:
      - alias: {name: ua-net0}
        model: {type: virtio}
      serials:
      - alias: {name: ua-ayatori-console}   # 控制台，CRD 默认值
        source: {pty: {}}
        target: {port: 0}
      tpms:
      - model: tpm-crb
        backend: {emulator: {version: "2.0"}}
```

- **`spec.domain` 就是交给 libvirt 的 domain**，Ayatori 只填写它拥有的字段（见下文"所有权"），不做其他
  改写或隐式添加。
- **`disks`、`interfaces` 只剩引用。** 用 libvirt 原生的用户 alias（必须以 `ua-` 开头）把 Ayatori 对象
  绑定到 `domain` 中的设备。原来的 `bus`、`bootOrder`、`readOnly`、`model`、`mac` 移入 `domain`，按
  libvirt 本来的写法表达。绑定的 alias 必须在 `domain` 中存在，一个设备只能绑定一次，由 CEL 校验。
  网卡必须全部绑定（没有 network 的网卡没有意义），`interfaces` 的顺序决定端口名；磁盘可以不绑定，即空光驱或
  空软驱，介质由运行时换盘（ADR-0016）。镜像是共享的副本，只能绑定到只读光驱，Agent 渲染时检查。
- **不用 `domain` 中的 `source.volume` 表达引用。** libvirt 的 `<source pool volume>` 有自己的含义（libvirt
  pool 中的卷），LINSTOR 卷没有 pool；把它解释为 Ayatori 对象就回到了 ADR-0015 要消除的方言。alias 在
  libvirt 中本来就是用户给设备起的名字，`domain` 中每个字段的含义都与 libvirt 相同。
- **删除 `firmware` 字段。** 固件和 vTPM 直接写在 `domain` 中（`os.firmware`、`os.firmwareInfo`、
  `devices.tpms`）；NVRAM 与 vTPM 状态放在哪里仍由 Ayatori 按实例的存储决定（ADR-0020）。

### 映射规则

一一对应的对象是 libvirtxml 的 `Domain` 模型，不是任意 XML：libvirtxml 表达不了的内容（语料中只有 25 个
样本，绝大多数是命名空间声明），YAML 也表达不了。

1. **key**：Go 字段名转 lowerCamel。开头由大写字母和数字组成的缩写整体小写；缩写后面紧跟小写字母时，
   最后一个大写字母属于下一个词；紧跟复数 `s` 时缩写整体小写。例如 `UUID`→`uuid`、`IOThreads`→`ioThreads`、
   `TPMs`→`tpms`、`SHA256`→`sha256`、`NVRam`→`nvRam`。key 名因此不总与 XML 名相同（`autoPort` 对应
   `autoport`）。
2. **值嵌入的结构体**展平到外层，生成时检查提升后同一层的 key 不冲突。
3. **有和没有要区分**：nil 指针省略；非 nil 指针一律保留，空结构体写成 `{}`（例如 `<acpi/>`），值为 0 或空串
   的标量照样写出（例如 `index: 0`）。值类型的零值与"没写"等价，这本来就是 libvirtxml 的语义。
4. **切片**映射为列表，顺序就是 XML 中的顺序。
5. **联合体**：libvirtxml 中 `source` 一类的联合体，每个分支是一个指针字段，映射为一个 key（`source.file`、
   `source.block`），分支名就是 XML 中 `type` 属性所表达的判别值。libvirtxml 序列化时只取第一个非空分支，
   其余**静默丢弃**（已验证）。两层保证：
   - schema：带 `xml:"-"` 标记、由自定义 marshaler 处理的指针字段视为同一联合体的分支，用 `oneOf` 限制最多
     一个，apply 时报错；
   - 下发前：Agent 把渲染结果序列化后再解析回来，与渲染前比较，有内容丢失就拒绝并写进 status。联合体的
     形式并不统一（`DomainAddress` 的分支没有标记，`DomainController` 的分支由 `type` 属性决定），这一层
     覆盖 schema 识别不了的情况，也覆盖 libvirtxml 其他会丢内容的地方。
   解析 XML 时 libvirtxml 会按 `type` 补上空的分支（没写 `type` 的 `<disk>` 读回为 `source.file: {}`），只多不少，
   不算丢失。
6. **递归**：只有磁盘的 `backingStore`（`DomainDiskSource`/`DomainDiskBackingStore`）一处，它属于 Ayatori
   拥有的 `source`，schema 中直接截断。
7. **未知 key** 由 apiserver 的字段校验拒绝（kubectl 与 server-side apply 默认 Strict）；Agent 转换时同样拒绝。

### 实现：类型就是 libvirtxml

- `spec.domain` 的 Go 类型是 `runtime.RawExtension`。转换函数（`internal/compute/domainspec`）用反射在
  `libvirtxml.Domain` 与 JSON 之间互转，Agent、compute controller 和将来 `Machine` 的 libvirt adapter 都用
  `libvirtxml.Domain` 读写，不生成第二套类型。
- schema 由同一套反射生成，`make manifests` 在 controller-gen 之后注入 CRD，所有权与绑定的 CEL 规则一起
  注入。生成时给每个列表加 `maxItems`、每个字符串加 `maxLength`，否则 CEL 的成本估算超限（spike 验证）。
- **生成的 schema 提交进仓库**（CRD 241KB JSON），CI 检查：
  - 重新生成后没有 diff（`make test` 之后的 `git diff --exit-code`）；
  - 双射：每个结构体（包括展平提升的字段）内 key 唯一，没有不支持的字段类型；
  - 往返：仓库内的代表性样本（q35 UEFI Linux、Windows、i440fx + IDE + SB16 + 软驱的 retro）经 YAML 往返后
    XML 不变。
  - 升级 libvirtxml 时用 hack 脚本对 libvirt 全量测试语料跑一次往返（spike：2511 个可解析样本全部逐字节
    一致）。
- **升级 libvirtxml 改了字段名，API 就随之改变。** schema 的 diff 会在 review 中暴露。v1alpha1 期间直接
  接受；需要兼容时在生成器中加改名覆盖表，不改写用户对象。
- **互转工具**：`go run ./hack/domainspec yaml < dom.xml` 与反向的 `xml`，用的是同一份转换函数。转换是一一对应的，
  不替用户删字段：`virsh dumpxml` 的输出中 Ayatori 拥有的字段（name、uuid、磁盘与网卡的 source 等）要自己删掉，
  apply 时 CEL 的报错会逐项指出。

### 所有权

以下字段由 Ayatori 填写，用户写了就由 CEL 在 apply 时拒绝，不再等到下发：

| 字段 | Ayatori 填写的内容 |
|---|---|
| `name`、`uuid` | 对象名与 UID |
| `metadata` | 下发记录的 generation 与输入 hash（ADR-0015"下发与漂移"） |
| `devices.disks[*].source`、`backingStore`、`mirror` | 绑定的卷或镜像解析出的路径与类型；未绑定的磁盘只能是空光驱或空软驱，介质由运行时换盘（ADR-0016） |
| `devices.disks[*].driver.type` | 卷的格式（raw、qcow2），是卷的事实；`driver` 的其他属性（cache、discard 等）由用户写 |
| `devices.interfaces[*].source`、`target` | 绑定的 network；端口名（`naming.PortName`，imds 按它识别实例，ADR-0016） |
| `os.loader` 的路径、`os.nvram` | 固件与 NVRAM 状态的位置 |
| `devices.tpms[*].backend.emulator.source` | vTPM 状态的位置 |
| `devices.graphics[*]` 的监听（地址、端口、socket） | 只监听本机 unix socket，由 Agent 中继（ADR-0016） |
| 写了 `userData` 时：`os.smBios`、SMBIOS 的 system serial | 指向元数据服务上的 NoCloud seed（ADR-0016）；SMBIOS 的其他内容由用户写 |

- **所有权以字段为单位，不以元素为单位。** 用户可以在同一列表里加自己的条目：第二个串口、额外的光驱都
  可以写。以后接管新的字段仍是不兼容变更，但范围缩小到具体字段。
- **控制台串口**用 alias `ua-ayatori-console` 标记，它的任何字段都不归 Ayatori：中继按 alias 经 libvirt 的
  console stream（`virDomainOpenConsole`）打开它，`pty` 即可（PoC 15）。放在哪个 `target.port`、要不要它由用户
  决定；删掉它，实例就没有串口控制台。
- 接口的 `mac` 不归 Ayatori：不写时由 Agent 从对象 UID 稳定生成，冷启动换节点后不变，与 ADR-0015 相同。
- 本 API 的写权限仍等同于宿主机 root（ADR-0015），上述规则只防止与 Ayatori 冲突，不是安全边界。

### 默认值

由 CRD 填写，因此在 spec 中可见：

- `os.firmware: efi`。要用 BIOS 显式写 `os.firmware: bios`（libvirt 原生支持该值）。
- `features.acpi: {}`：UEFI 必须有 ACPI，否则 libvirt 拒绝 define。今天由 Agent 隐式添加，改为默认值。
- `devices.serials`：一个 `ua-ayatori-console` 的 `pty` 串口，`target.port: 0`。

CRD 的默认值只在字段缺失时生效：用户写了 `devices.serials` 就不再补控制台串口，写了 `features` 就不再补
`acpi`。父对象缺失时（例如没写 `os` 或 `devices`），父对象的默认值写成完整的对象，不依赖嵌套默认值。Agent 不做
任何默认值以外的隐式添加，原来 `renderFirmware` 中补 ACPI 与 secure-boot feature 的逻辑随之删除。

### 其他

- **容量与放置**：controller 直接读 `domain` 中的 `memory`、`vcpu`、`cpu`，不再解析 XML。
- **libvirt 的语义校验**（例如某种总线不支持某个设备）仍在 define 时发生，错误写进 status，与今天相同。
- **不在本 ADR 的范围**：
  - `LXCContainer` 的 `config` 是 key=value 文本，问题不同，保持原样；
  - pool、network、volume 的原生 XML 不经 Ayatori 改写，没有上述问题，保持原样。

## 待验证（Accepted 之前必须完成）

1. **联合体限制**：**已完成（2026-11-12）**。机械识别全部联合体做不到（见"映射规则"第 5 条），改为 schema 中
   `oneOf` 加下发前的往返检查。往返检查在 libvirt 12.8.0 的全部 2511 个可解析样本上通过；发现 libvirtxml 的
   `DomainNVRam.UnmarshalXML` 有一处清空不生效的 bug（`<nvram>` 内的空白残留），只影响解析 XML，测试中补正。
2. **嵌套默认值**：**已完成**。父对象缺失时用完整对象作默认值；envtest 验证了缺省时补上、写了时不覆盖。
3. **所有权与绑定的 CEL 规则**：**已完成**。全部规则在 `maxItems` 下的成本估算通过，CRD 被 apiserver 接受；envtest
   逐条验证拒绝。
4. **CRD 的安装方式**：**已完成**。CRD 经 Flux 的 kustomize-controller 安装（`deploy/flux/crds`），它使用
   server-side apply。
5. **端到端**：**已完成（2026-11-12）**。compute-dev1 上用只写了机型、内存、CPU、一块盘和一块网卡的 spec 创建
   Debian 13 VM：CRD 补上 `os.firmware: efi`、`features.acpi` 与控制台串口；libvirt 自动选中 OVMF，客户机内
   `/sys/firmware/efi` 存在；经 `ua-ayatori-console` 串口登录；客户机内 imds 返回 instance-id（对象 UID）与 name，
   不带 token 返回 401。

## 结果

- 修订 ADR-0015：
  - `LibvirtDomain` 的 `domainXML` 改为结构化的 `domain`，`firmware` 字段删除，`disks`/`interfaces` 只剩引用；
  - Ayatori 拥有的内容从"元素出现即拒绝"改为按字段的所有权，由 CEL 在 apply 时校验；
  - 评估中"从 libvirt schema 生成完整的 YAML API"的否决由本 ADR 取代。
- `LibvirtDomain` 的 API 不兼容变更。目前是 v1alpha1，dev 环境中没有 VM 对象，代价最小。
- 用户写的 key 与 libvirt 文档不完全一致（`autoPort`、`source.file.file`），靠互转工具和 `kubectl explain`
  缓解。
- API 的表达力与 libvirtxml 绑定，与今天相同，只是边界前移到了 apiserver。
