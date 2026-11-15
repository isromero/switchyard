#!/usr/bin/env bash
# Used by action.yml: find or download the switchyard binary, then run
# `switchyard pick`. Any failure prints a warning and outputs the
# fallback label, so CI never blocks on this step.
set -o pipefail

# Used only when switchyard itself cannot run; otherwise the config's
# fallback applies unless the fallback input overrides it.
fallback="${INPUT_FALLBACK:-ubuntu-latest}"

# runners_json prints the runners output with every job from INPUT_JOBS
# ("check, test:medium*5") set to the fallback. Same rules as switchyard's
# own parser; anything it can't read gives {}.
runners_json() {
  local - out="" item name count k
  set -f # items contain '*'; don't expand them as file names
  for item in $(printf '%s' "${INPUT_JOBS:-}" | tr ',\n\t\r' '    '); do
    count=0
    case "$item" in
      *'*'*) count="${item##*\*}"; item="${item%\*"$count"}" ;;
    esac
    name="${item%%:*}"
    case "$name" in ''|*[!A-Za-z0-9_.-]*) echo '{}'; return ;; esac
    case "$count" in ''|*[!0-9]*) echo '{}'; return ;; esac
    if [ "$count" -eq 0 ]; then
      out="$out,\"$name\":\"$fallback\""
    else
      [ "$count" -le 64 ] || { echo '{}'; return; }
      for ((k = 1; k <= count; k++)); do out="$out,\"$name-$k\":\"$fallback\""; done
    fi
  done
  echo "{${out#,}}"
}

fail_open() {
  trap - EXIT
  echo "::warning title=switchyard::$1; using fallback $fallback"
  {
    echo "runner=$fallback"
    echo "provider="
    echo "paid=false"
    echo "fallback=true"
    for s in small medium large; do echo "runner_$s=$fallback"; done
    echo "runners=$(runners_json)"
  } >> "$GITHUB_OUTPUT"
  exit 0
}

# Safety net: whatever goes wrong, the step succeeds with the fallback.
trap 'fail_open "unexpected error in pick.sh"' EXIT

repo="${ACTION_REPO:-}"
[ -n "$repo" ] || fail_open "github.action_repository is empty; cannot find releases"
api="${GITHUB_API_URL:-https://api.github.com}"
dl_token="${INPUT_DOWNLOAD_TOKEN:-$INPUT_TOKEN}"

case "${RUNNER_OS:-}" in
  Linux) os=linux ;;
  macOS) os=darwin ;;
  *) fail_open "unsupported runner OS '${RUNNER_OS:-}'" ;;
esac
case "${RUNNER_ARCH:-}" in
  X64) arch=amd64 ;;
  ARM64) arch=arm64 ;;
  *) fail_open "unsupported runner arch '${RUNNER_ARCH:-}'" ;;
esac
asset="switchyard_${os}_${arch}.tar.gz"

want="${INPUT_VERSION:-${ACTION_REF:-latest}}"
# A full commit SHA (the action pinned by SHA): use the release whose
# vX.Y.Z tag points at that commit, so pinning the action pins the binary.
# If no such tag can be found, don't substitute another release.
if [[ "$want" =~ ^[0-9a-f]{40}$ ]]; then
  sha="$want"
  want=""
  url="$api/repos/$repo/tags?per_page=100"
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    tags="$(curl -fsSL --retry 2 --max-time 20 \
      -H "Authorization: Bearer $dl_token" -H "Accept: application/vnd.github+json" \
      "$url")" || fail_open "cannot list tags of $repo to resolve $sha"
    if command -v jq >/dev/null 2>&1; then
      t="$(printf '%s' "$tags" | jq -r --arg sha "$sha" '[.[] | select(.commit.sha == $sha) | .name | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))][0] // ""')"
    elif command -v python3 >/dev/null 2>&1; then
      t="$(printf '%s' "$tags" | python3 -I -c '
import json, re, sys
for x in json.load(sys.stdin):
    if x["commit"]["sha"] == sys.argv[1] and re.fullmatch(r"v\d+\.\d+\.\d+", x["name"]):
        print(x["name"]); break
' "$sha")"
    else
      fail_open "need jq or python3 to read the tag list"
    fi
    if [ -n "$t" ]; then want="$t"; break; fi
    [ "$(printf '%s' "$tags" | head -c 2)" != "[]" ] || break
    case "$url" in
      *"&page="*) n="${url##*&page=}"; url="${url%&page=*}&page=$((n + 1))" ;;
      *) url="$url&page=2" ;;
    esac
  done
  [ -n "$want" ] || fail_open "no vX.Y.Z release tag points at $sha; set the version input"
fi
cache_root="${RUNNER_TOOL_CACHE:-${HOME}/.cache}/switchyard"

# An exact version that is already downloaded needs no API call.
bin=""
if [[ "$want" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] && [ -x "$cache_root/$want/$os-$arch/switchyard" ]; then
  bin="$cache_root/$want/$os-$arch/switchyard"
fi

if [ -z "$bin" ]; then
  releases="$(curl -fsSL --retry 2 --max-time 20 \
    -H "Authorization: Bearer $dl_token" -H "Accept: application/vnd.github+json" \
    "$api/repos/$repo/releases?per_page=100")" || fail_open "cannot list releases of $repo (does download-token have Contents: read on it?)"

  # Pick the release: exact tag, newest vN.x.y for a major tag, else newest.
  if command -v jq >/dev/null 2>&1; then
    found="$(printf '%s' "$releases" | jq -r --arg want "$want" --arg asset "$asset" '
      [ .[] | select((.draft or .prerelease) | not)
        | select(
            if ($want | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) then .tag_name == $want
            elif ($want | test("^v[0-9]+$")) then (.tag_name | startswith($want + "."))
            else true end) ][0]
      | select(. != null)
      | [.tag_name,
         ((.assets[] | select(.name == $asset) | .url) // ""),
         ((.assets[] | select(.name == "checksums.txt") | .url) // "")] | @tsv')"
  elif command -v python3 >/dev/null 2>&1; then
    found="$(printf '%s' "$releases" | python3 -I -c '
import json, re, sys
want, asset = sys.argv[1], sys.argv[2]
for r in json.load(sys.stdin):
    if r.get("draft") or r.get("prerelease"):
        continue
    t = r["tag_name"]
    if re.fullmatch(r"v\d+\.\d+\.\d+", want) and t != want:
        continue
    if re.fullmatch(r"v\d+", want) and not t.startswith(want + "."):
        continue
    url = next((a["url"] for a in r.get("assets", []) if a["name"] == asset), "")
    sums = next((a["url"] for a in r.get("assets", []) if a["name"] == "checksums.txt"), "")
    print(t + "\t" + url + "\t" + sums)
    break
' "$want" "$asset")"
  else
    fail_open "need jq or python3 to read the release list"
  fi

  tag="$(printf '%s' "$found" | cut -f1)"
  asset_url="$(printf '%s' "$found" | cut -f2)"
  sums_url="$(printf '%s' "$found" | cut -f3)"
  [ -n "$tag" ] || fail_open "no release of $repo matches '$want'"
  [ -n "$asset_url" ] || fail_open "release $tag has no asset $asset"
  [ -n "$sums_url" ] || fail_open "release $tag has no checksums.txt; not running an unverified binary"

  dir="$cache_root/$tag/$os-$arch"
  bin="$dir/switchyard"
  if [ ! -x "$bin" ]; then
    mkdir -p "$dir" || fail_open "cannot create $dir"
    tmp="$(mktemp -d)"
    for pair in "$asset_url $tmp/$asset" "$sums_url $tmp/checksums.txt"; do
      read -r u f <<<"$pair"
      curl -fsSL --retry 2 --max-time 60 -o "$f" \
        -H "Authorization: Bearer $dl_token" -H "Accept: application/octet-stream" \
        "$u" || { rm -rf "$tmp"; fail_open "cannot download $(basename "$f") from $tag"; }
    done
    expected="$(awk -v a="$asset" '$2 == a || $2 == "*" a {print $1}' "$tmp/checksums.txt")"
    if command -v sha256sum >/dev/null 2>&1; then
      actual="$(sha256sum "$tmp/$asset" | cut -d' ' -f1)"
    else
      actual="$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)"
    fi
    if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
      rm -rf "$tmp"
      fail_open "checksum of $asset from $tag does not match checksums.txt; not running it"
    fi
    tar -xzf "$tmp/$asset" -C "$tmp" || { rm -rf "$tmp"; fail_open "cannot unpack $asset"; }
    if ! { mv "$tmp/switchyard" "$bin.tmp" && chmod +x "$bin.tmp" && mv "$bin.tmp" "$bin"; }; then
      rm -rf "$tmp"
      fail_open "cannot install $bin"
    fi
    rm -rf "$tmp"
  fi
fi

config="${INPUT_CONFIG:-}"
if [ -z "$config" ]; then
  if [ -f "${GITHUB_WORKSPACE:-.}/.github/switchyard.yml" ]; then
    config="${GITHUB_WORKSPACE:-.}/.github/switchyard.yml"
  else
    config="$ACTION_PATH/switchyard.yml"
  fi
fi

export SWITCHYARD_TOKEN="$INPUT_TOKEN"
if [ -n "${INPUT_RUNNERS_TOKEN:-}" ]; then export SWITCHYARD_RUNNERS_TOKEN="$INPUT_RUNNERS_TOKEN"; fi
export SWITCHYARD_CONFIG="$config"
if [ -n "${INPUT_FALLBACK:-}" ]; then export SWITCHYARD_FALLBACK="$INPUT_FALLBACK"; fi
export SWITCHYARD_DISABLE="${INPUT_DISABLE:-}"
export SWITCHYARD_JOBS="${INPUT_JOBS:-}"
if [ -n "${CACHE_FILE:-}" ]; then export SWITCHYARD_CACHE_FILE="$CACHE_FILE"; fi

"$bin" pick --size "${INPUT_SIZE:-small}" --arch "${INPUT_ARCH:-any}" >/dev/null
code=$?
trap - EXIT
case "$code" in
  0) ;;
  3) exit 1 ;; # on_exhausted: fail; switchyard already logged why
  *) fail_open "switchyard exited with code $code" ;;
esac
