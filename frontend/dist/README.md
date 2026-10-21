# frontend/dist —— 可选的界面素材目录

这个目录会被编译进程序（`main.go` 里 `//go:embed all:frontend/dist`），但**里面的图片不随仓库分发**。

## 为什么留着这个文件

`go:embed` 要求目录里有文件；把这个说明文件留在版本库里，clone 下来也能直接编译。

## 想放自己的素材？把文件丢进本目录即可，代码会自动识别

| 文件名 | 用途 | 没有时 |
|---|---|---|
| `zanshang-qr.png` | 「赞赏支持」弹窗里的赞赏码 | 点赞赏会提示"没打包进来" |
| `weixin-qr.png` | 个人微信二维码 | 服务大厅只显示微信号文字 |
| `wxgroup-qr.png` | 微信群二维码 | 回退到个人微信码 |
| `qqgroup-qr.png` | QQ 群二维码 | 只显示群号 |
| `dashu-avatar.png` | 服务方头像 | 用「叔」字占位 |

文件名与开关都在 `service.go` 顶部的常量里（`svcQrName` / `svcRewardName` / `svcWxQrGroup` / `svcQQQrName` / `svcAvatarName`）。

> 说明：本站作者的个人二维码与联系方式**不在**开源仓库里，仓库只包含程序本身。
