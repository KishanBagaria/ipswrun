#!/bin/bash
# Runs a guest against every restore image in examples/builds.txt and diffs what each run reported
# against what is recorded for it: examples/expected/<build>.json for examples/guest.c, and
# examples/expected/<guest>/<build>.json for the others.
#
#   examples/matrix.sh [--guest=NAME] [--stream-compressed] [--update] [version prefix...]
#
#   --guest=NAME         foundation, swiftrt, objcinfo or crypto (see examples/Makefile) instead of guest.c
#   --stream-compressed  also run macOS 13 and 14, which download a 3.6–4.5 GB member each
#   --update             write the results to examples/expected instead of diffing against it
#   version prefix       only the builds whose version starts with one of these (e.g. 15. 27.2)
#
# Downloads are kept per build in $IPSWRUN_CACHE (default ~/Library/Caches/ipswrun/matrix), so a
# second run fetches nothing. Each run's report, output and diagnostics are left in $RESULTS
# (default a temporary directory). Needs jq. Exits 1 if any build differs from what is expected.
set -u
cd "$(dirname "$0")/.."

guest=guest stream= update= prefixes=()
for arg; do
	case $arg in
	--guest=*) guest=${arg#--guest=} ;;
	--stream-compressed) stream=1 ;;
	--update) update=1 ;;
	-*) sed -n '2,15s/^# \{0,1\}//p' "$0"; exit 2 ;;
	*) prefixes+=("$arg") ;;
	esac
done

cache=${IPSWRUN_CACHE:-$HOME/Library/Caches/ipswrun/matrix}
results=${RESULTS:-$(mktemp -d -t ipswrun-matrix)}
mkdir -p "$results/bin" || exit
go build -o "$results/bin/ipswrun" ./cmd/ipswrun || exit
expected=examples/expected args=()
if [ "$guest" = guest ]; then
	clang -arch arm64 -mmacosx-version-min=11.0 -Wl,-no_fixup_chains -o "$results/bin/guest" examples/guest.c -lsqlite3 || exit
else
	make -s -C examples "bin/$guest" && cp "examples/bin/$guest" "$results/bin/guest" || exit
	expected=examples/expected/$guest
	# objcinfo describes the frameworks it is given: one that only exists as a private framework.
	[ "$guest" != objcinfo ] || args=(/System/Library/PrivateFrameworks/IMCore.framework/IMCore)
fi

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
		"$url" "$results/bin/guest" ${args[@]+"${args[@]}"} >"$results/$build.out" 2>"$results/$build.err" </dev/null
	status=$?

	# What a build is expected to reproduce: its identity, the guest's exit status and its output.
	# Transfer sizes and timings depend on the cache and the network, so they are left out. A run
	# that stopped wrote no report; its status (70) stands in.
	[ -s "$report" ] || echo '{}' >"$report"
	jq -S --arg version "$version" --arg build "$build" --argjson status "$status" --rawfile output "$results/$build.out" \
		'{version: (.version // $version), build: (.build // $build), cache_uuid, exit_code: (.exit_code // $status), output: $output}' \
		"$report" >"$results/$build.json"

	if [ -n "$update" ]; then
		mkdir -p "$expected" && cp "$results/$build.json" "$expected/"
		verdict=recorded
	elif [ ! -e "$expected/$build.json" ]; then
		verdict='no expectation (run with --update)' failed=1
	elif diff -u "$expected/$build.json" "$results/$build.json" >"$results/$build.diff"; then
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
