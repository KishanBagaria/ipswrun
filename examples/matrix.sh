#!/bin/bash
# Runs examples/guest.c against every restore image in examples/builds.txt and diffs what each run
# reported against examples/expected/<build>.json.
#
#   examples/matrix.sh [--stream-compressed] [--update] [version prefix...]
#
#   --stream-compressed  also run macOS 13 and 14, which download a 3.6–4.5 GB member each
#   --update             write the results to examples/expected instead of diffing against it
#   version prefix       only the builds whose version starts with one of these (e.g. 15. 27.2)
#
# Downloads are kept per build in $IPSWRUN_CACHE (default ~/Library/Caches/ipswrun/matrix), so a
# second run fetches nothing. Each run's report, output and diagnostics are left in $RESULTS
# (default a temporary directory). Needs jq. Exits 1 if any build differs from what is expected.
set -u
cd "$(dirname "$0")/.."

stream= update= prefixes=()
for arg; do
	case $arg in
	--stream-compressed) stream=1 ;;
	--update) update=1 ;;
	-*) sed -n '2,13s/^# \{0,1\}//p' "$0"; exit 2 ;;
	*) prefixes+=("$arg") ;;
	esac
done

cache=${IPSWRUN_CACHE:-$HOME/Library/Caches/ipswrun/matrix}
results=${RESULTS:-$(mktemp -d -t ipswrun-matrix)}
mkdir -p "$results/bin" || exit
go build -o "$results/bin/ipswrun" ./cmd/ipswrun || exit
clang -arch arm64 -mmacosx-version-min=11.0 -Wl,-no_fixup_chains -o "$results/bin/guest" examples/guest.c -lsqlite3 || exit

failed=0
while IFS=$'\t' read -r version build url; do
	case $version in '' | \#*) continue ;; esac
	if [ ${#prefixes[@]} -gt 0 ]; then
		wanted=
		for prefix in "${prefixes[@]}"; do
			case $version in "$prefix"*) wanted=1 ;; esac
		done
		[ -n "$wanted" ] || continue
	fi
	flags=()
	case $version in 13.* | 14.*)
		if [ -z "$stream" ]; then
			printf '%-8s %-8s skipped (needs --stream-compressed)\n' "$version" "$build"
			continue
		fi
		flags=(--stream-compressed)
		;;
	esac

	home=$results/$build.home report=$results/$build.report.json
	rm -rf "$home" "$report"
	mkdir -p "$home" "$cache/$build"
	# ${flags[@]+…}: an empty array is "unbound" to the bash 3.2 that macOS ships.
	"$results/bin/ipswrun" ${flags[@]+"${flags[@]}"} --cache "$cache/$build" --home "$home" --report "$report" \
		"$url" "$results/bin/guest" >"$results/$build.out" 2>"$results/$build.err" </dev/null
	status=$?

	# What a build is expected to reproduce: its identity, the guest's exit status and its output.
	# Transfer sizes and timings depend on the cache and the network, so they are left out. A run
	# that stopped wrote no report; its status (70) stands in.
	[ -s "$report" ] || echo '{}' >"$report"
	jq -S --arg version "$version" --arg build "$build" --argjson status "$status" --rawfile output "$results/$build.out" \
		'{version: (.version // $version), build: (.build // $build), cache_uuid, exit_code: (.exit_code // $status), output: $output}' \
		"$report" >"$results/$build.json"

	if [ -n "$update" ]; then
		mkdir -p examples/expected && cp "$results/$build.json" examples/expected/
		verdict=recorded
	elif [ ! -e "examples/expected/$build.json" ]; then
		verdict='no expectation (run with --update)' failed=1
	elif diff -u "examples/expected/$build.json" "$results/$build.json" >"$results/$build.diff"; then
		verdict=ok
	else
		verdict=DIFFERS failed=1
	fi
	printf '%-8s %-8s exit %-3s %s\n' "$version" "$build" "$status" "$verdict"
	[ "$verdict" != DIFFERS ] || sed 's/^/    /' "$results/$build.diff"
	[ "$status" -ne 70 ] || tail -n 5 "$results/$build.err" | sed 's/^/    /'
done <examples/builds.txt

echo "results in $results"
exit $failed
