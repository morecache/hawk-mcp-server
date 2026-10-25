#!/usr/bin/env bash
# Compare hawk-apiserver's /api/mcp/v1 route table with this repo's openapi.yaml.
#
# This is a static check: it reads the apiserver's main.go and openapi.yaml,
# prints a path-by-path table, and exits non-zero when a path exists on one
# side only. It does not start the server or contact the cluster.
#
# The spec is parsed with awk using the indentation of the current file:
# paths at two spaces, methods at four, operationId at six. Keep that layout.
set -uo pipefail

APISERVER="../hawk-apiserver"
SPEC="openapi.yaml"
QUIET=0

usage() {
    cat <<'EOF'
Usage: scripts/check-contract.sh [--apiserver PATH] [--spec FILE] [--quiet]

  --apiserver PATH  hawk-apiserver source checkout, contains main.go
                    (default: ../hawk-apiserver)
  --spec FILE       OpenAPI document (default: openapi.yaml)
  --quiet           print only the summary (for CI logs)
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --apiserver) APISERVER="$2"; shift 2 ;;
        --spec) SPEC="$2"; shift 2 ;;
        --quiet) QUIET=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
done

PREFIX="/api/mcp/v1"
MAIN_GO="$APISERVER/main.go"

[ -f "$SPEC" ] || { echo "spec not found: $SPEC" >&2; exit 2; }
[ -f "$MAIN_GO" ] || { echo "apiserver main.go not found: $MAIN_GO (pass --apiserver <checkout>)" >&2; exit 2; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# path <TAB> METHOD <TAB> operationId
awk '
    /^  \/api\/mcp\/v1/ {
        path=$1; sub(/:$/, "", path); inpath=1; method=""; next
    }
    inpath && /^    (get|post|put|delete|patch):[[:space:]]*$/ {
        method=toupper($1); sub(/:$/, "", method); next
    }
    inpath && /^      operationId:/ {
        if (method != "") print path "\t" method "\t" $2
        next
    }
    inpath && /^  [^ ]/ { inpath=0 }
' "$SPEC" > "$tmp/ops.tsv"

[ -s "$tmp/ops.tsv" ] || { echo "no $PREFIX operations in $SPEC" >&2; exit 2; }

dups=$(cut -f3 "$tmp/ops.tsv" | sort | uniq -d)
if [ -n "$dups" ]; then
    echo "duplicate operationIds: $(echo "$dups" | tr '\n' ' ')" >&2
    exit 1
fi

cut -f1 "$tmp/ops.tsv" | sort -u > "$tmp/paths.txt"

grep -oE 'HandleFunc\("/api/mcp/v1[^"]*"' "$MAIN_GO" \
    | sed 's/^HandleFunc("//; s/"$//' > "$tmp/routes.txt" || true

declare -A exact=() prefixes=() used=() prefix_count=()
while IFS= read -r route; do
    [ -n "$route" ] || continue
    relative=${route#"$PREFIX"}
    [ -n "$relative" ] && [ "$relative" != "/" ] || continue
    if [[ "$relative" == */ ]]; then
        prefixes["$relative"]=1
    else
        exact["$relative"]=1
    fi
done < "$tmp/routes.txt"

rows=()
missing=()
while IFS= read -r path; do
    relative=${path#"$PREFIX"}
    served=""
    if [ -n "${exact["$relative"]+x}" ]; then
        served="exact: $PREFIX$relative"
    else
        for p in "${!prefixes[@]}"; do
            if [[ "$relative" == "$p"* && ${#relative} -gt ${#p} ]]; then
                served="prefix: $PREFIX$p"
                used["$p"]=1
                prefix_count["$p"]=$(( ${prefix_count["$p"]:-0} + 1 ))
                break
            fi
        done
    fi
    if [ -z "$served" ]; then
        served="MISSING"
        missing+=("$path")
    fi
    ops=$(awk -F'\t' -v p="$path" '
        $1 == p { printf "%s%s %s", sep, $2, $3; sep=", " }
    ' "$tmp/ops.tsv")
    rows+=("$path"$'\t'"$ops"$'\t'"$served")
done < "$tmp/paths.txt"

extra=()
for relative in "${!exact[@]}"; do
    if ! grep -qxF "$PREFIX$relative" "$tmp/paths.txt"; then
        extra+=("$relative")
    fi
done

unused=()
for p in "${!prefixes[@]}"; do
    [ -n "${used["$p"]+x}" ] || unused+=("$p")
done

sort_array() {
    if [ $# -eq 0 ]; then return; fi
    printf '%s\n' "$@" | sort
}

operation_count=$(wc -l < "$tmp/ops.tsv")
path_count=$(wc -l < "$tmp/paths.txt")
matched=$((path_count - ${#missing[@]}))

print_table() {
    local headers=("#" "openapi path" "operations" "served by apiserver")
    local w0=${#headers[0]} w1=${#headers[1]} w2=${#headers[2]} w3=${#headers[3]}
    local n=1 path ops served
    for row in "${rows[@]}"; do
        IFS=$'\t' read -r path ops served <<< "$row"
        [ ${#path} -gt $w1 ] && w1=${#path}
        [ ${#ops} -gt $w2 ] && w2=${#ops}
        [ ${#served} -gt $w3 ] && w3=${#served}
        n=$((n+1))
    done
    [ ${#n} -gt $w0 ] && w0=${#n}

    printf '  %-*s  %-*s  %-*s  %s\n' "$w0" "${headers[0]}" "$w1" "${headers[1]}" "$w2" "${headers[2]}" "${headers[3]}"
    printf '  %-*s  %-*s  %-*s  %-*s\n' "$w0" "$(printf '%*s' "$w0" '' | tr ' ' '-')" \
        "$w1" "$(printf '%*s' "$w1" '' | tr ' ' '-')" \
        "$w2" "$(printf '%*s' "$w2" '' | tr ' ' '-')" \
        "$w3" "$(printf '%*s' "$w3" '' | tr ' ' '-')"
    n=1
    for row in "${rows[@]}"; do
        IFS=$'\t' read -r path ops served <<< "$row"
        printf '  %-*s  %-*s  %-*s  %s\n' "$w0" "$n" "$w1" "$path" "$w2" "$ops" "$served"
        n=$((n+1))
    done
}

if [ "$QUIET" -eq 0 ]; then
    cat <<'EOF'
What this checks
  openapi.yaml lists the operations the MCP server advertises; each
  operationId becomes one MCP tool. The apiserver's main.go lists the
  /api/mcp/v1 routes it actually serves. This script compares the two.
  "exact" means one handler for that path; "prefix" means one handler
  serves every path under it (for example /resources/<id>/start).

Path-by-path comparison
EOF
    print_table
    echo
fi

echo "Summary"
printf '  openapi.yaml:      %d operations on %d paths\n' "$operation_count" "$path_count"
prefix_suffix=""
if [ ${#prefixes[@]} -gt 0 ]; then
    prefix_names=$(for p in "${!prefixes[@]}"; do
        printf '%d\t%s\n' "${prefix_count["$p"]:-0}" "$p"
    done | sort -rn -k1,1 -k2,2 | cut -f2 | paste -sd ',' - | sed 's/,/, /g')
    prefix_suffix=" ($prefix_names)"
fi
printf '  apiserver routes:  %d exact + %d prefix%s\n' "${#exact[@]}" "${#prefixes[@]}" "$prefix_suffix"
printf '  matched:           %d/%d paths\n' "$matched" "$path_count"

if [ ${#missing[@]} -eq 0 ] && [ ${#extra[@]} -eq 0 ] && [ ${#unused[@]} -eq 0 ]; then
    echo "  result:            OK - every advertised path has an apiserver route"
    exit 0
fi

echo "  result:            MISMATCH"
echo
echo "Problems"
if [ ${#missing[@]} -gt 0 ]; then
    echo "  In openapi.yaml but not served by the apiserver:"
    for path in "${missing[@]}"; do
        echo "    !! $path"
    done
fi
if [ ${#extra[@]} -gt 0 ]; then
    echo "  Served by the apiserver but missing from openapi.yaml:"
    while IFS= read -r path; do
        echo "    !! $PREFIX$path"
    done < <(sort_array "${extra[@]}")
fi
if [ ${#unused[@]} -gt 0 ]; then
    echo "  Apiserver prefix routes that match no spec path:"
    while IFS= read -r path; do
        echo "    !! $PREFIX$path"
    done < <(sort_array "${unused[@]}")
fi
echo
echo "  Fix: add the route in apiserver/main.go, or add/remove the operation in"
echo "  openapi.yaml, then run this check again. Keep both repositories in sync."
exit 1
