#!/bin/sh
# 把代码、tag 和发布同步到 Gitee 镜像（国内用户检查更新、下载安装包走这里）。
# 用法：VERSION=x.y.z NOTES=发布说明.md build/gitee.sh
# 前提：GitHub 上已发布、dist/ 里有三个安装包；本机 SSH 公钥已加到 Gitee（git remote gitee）。
# 建发行版、传安装包要用 Gitee 私人令牌（勾选 projects），放在 ~/.gitee_token；没有令牌时只推代码，安装包到网页上手动传。
set -eu
cd "$(dirname "$0")/.."

VERSION=${VERSION:?需要 VERSION}
NOTES=${NOTES:?需要 NOTES（发布说明文件，末尾带 SHA-256 段落）}
REPO=sun_xuanqi/modbus_ai_studio
API=https://gitee.com/api/v5/repos/$REPO
FILES="ModbusAIStudio-$VERSION-Windows-x64.zip ModbusAIStudio-$VERSION-macOS-AppleSilicon.dmg ModbusAIStudio-$VERSION-macOS-Intel.dmg"

git remote get-url gitee >/dev/null 2>&1 || git remote add gitee "git@gitee.com:$REPO.git"
for i in 1 2 3 4 5; do
	if git push gitee main:main "v$VERSION"; then
		break
	fi
	echo "推送重试 $i"
	sleep 10
done

if [ ! -s "$HOME/.gitee_token" ]; then
	echo "没有 ~/.gitee_token：请在 https://gitee.com/$REPO/releases/new 选 tag v$VERSION，"
	echo "标题“Modbus AI Studio $VERSION”，说明粘贴 $NOTES 的内容，上传：$FILES"
	exit 0
fi
TOKEN=$(tr -d ' \r\n' < "$HOME/.gitee_token")

# 建发布（已存在就用现有的），再逐个上传安装包
ID=$(curl -s "$API/releases/tags/v$VERSION?access_token=$TOKEN" | python3 -c 'import sys,json
try: print(json.load(sys.stdin).get("id") or "")
except Exception: print("")')
if [ -z "$ID" ]; then
	ID=$(curl -sf -X POST "$API/releases" -F "access_token=$TOKEN" -F "tag_name=v$VERSION" \
		-F "name=Modbus AI Studio $VERSION" -F "body=<$NOTES" -F "target_commitish=main" |
		python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')
fi
echo "Gitee 发布 v$VERSION（id $ID）"
for f in $FILES; do
	for i in 1 2 3 4 5; do
		if curl -sf "$API/releases/$ID/attach_files?access_token=$TOKEN" | grep -q "\"name\":\"$f\""; then
			echo "已上传 $f"
			break
		fi
		curl -sf -X POST "$API/releases/$ID/attach_files" -F "access_token=$TOKEN" -F "file=@dist/$f" >/dev/null || { echo "上传重试 $f（$i）"; sleep 10; }
	done
done
echo "https://gitee.com/$REPO/releases/tag/v$VERSION"
