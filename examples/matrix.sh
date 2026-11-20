#!/bin/bash
# Runs a guest against every release that "all" stands for (ipswrun --releases) and diffs what each run reported
# against what is recorded for it in examples/expected/<guest>/<build>.json.
#
#   examples/matrix.sh [--guest=NAME,...] [--jobs=N] [--stream-compressed] [--update] [version prefix...]
#
#   --guest=NAME,...     of GUESTS in examples/Makefile, instead of guest; all for every one
#   --jobs=N             releases run at once (default: one per processor)
#   --stream-compressed  also run macOS 13 and 14, which download a 3.6–4.5 GB member each
#   --update             write the results to examples/expected instead of diffing against it
#   version prefix       only the builds whose version starts with one of these (e.g. 15. 27.2)
#
# Downloads are kept per build in $IPSWRUN_CACHE (default ~/Library/Caches/ipswrun/matrix), so a
# second run fetches nothing. Each run's report, output and diagnostics are left in $RESULTS/<guest>
# (default a temporary directory). Needs jq. Exits 1 if any build differs from what is expected.
# The guests are built on a Mac; anywhere else they have to be in examples/bin already.
set -u
cd "$(dirname "$0")/.."

guests=guest jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1) stream= update= prefixes=()
for arg; do
	case $arg in
	--guest=*) guests=${arg#--guest=} ;;
	--jobs=*) jobs=${arg#--jobs=} ;;
	--stream-compressed) stream=1 ;;
	--update) update=1 ;;
	-*) sed -n '2,16s/^# \{0,1\}//p' "$0"; exit 2 ;;
	*) prefixes+=("$arg") ;;
	esac
done

cache=${IPSWRUN_CACHE:-$HOME/Library/Caches/ipswrun/matrix}
results=${RESULTS:-$(mktemp -d "${TMPDIR:-/tmp}/ipswrun-matrix.XXXXXX")} || exit
mkdir -p "$results/bin" || exit
go build -o "$results/bin/ipswrun" ./cmd/ipswrun || exit
[ "$guests" != all ] || guests=$(sed -n 's/^GUESTS = //p' examples/Makefile)

# The releases to run, as "version build" lines, and those left out.
releases=() skipped=()
while IFS=$'\t' read -r version build _; do
	if [ ${#prefixes[@]} -gt 0 ]; then
		wanted=
		for prefix in "${prefixes[@]}"; do
			case $version in "$prefix"*) wanted=1 ;; esac
		done
		[ -n "$wanted" ] || continue
	fi
	case $version in 13.* | 14.*)
		if [ -z "$stream" ]; then
			skipped+=("$(printf '%-8s %-8s skipped (needs --stream-compressed)' "$version" "$build")")
			continue
		fi
		;;
	esac
	releases+=("$version $build")
done < <("$results/bin/ipswrun" --releases)

# run_one runs $guest against one release and leaves its verdict, and what to show with it, in
# $out/$build.verdict. Each release has its own cache directory, so several can run at once.
run_one() {
	local version=$1 build=$2 flags=() verdict status
	case $version in 13.* | 14.*) flags=(--stream-compressed) ;; esac
	local home=$out/$build.home report=$out/$build.report.json
	rm -rf "$home" "$report"
	mkdir -p "$home" "$cache/$build"
	# ${flags[@]+…}: an empty array is "unbound" to the bash 3.2 that macOS ships.
	"$results/bin/ipswrun" ${flags[@]+"${flags[@]}"} ${options[@]+"${options[@]}"} --cache "$cache/$build" --home "$home" --report "$report" \
		"$build" "examples/bin/$guest" ${args[@]+"${args[@]}"} >"$out/$build.out" 2>"$out/$build.err" </dev/null
	status=$?

	# What a build is expected to reproduce: its identity, the guest's exit status and its output.
	# Transfer sizes and timings depend on the cache and the network, so they are left out. A run
	# that stopped wrote no report; its status (70) stands in.
	[ -s "$report" ] || echo '{}' >"$report"
	jq -S --arg version "$version" --arg build "$build" --argjson status "$status" --rawfile output "$out/$build.out" \
		'{version: (.version // $version), build: (.build // $build), cache_uuid, exit_code: (.exit_code // $status), output: $output}' \
		"$report" >"$out/$build.json"

	if [ -n "$update" ]; then
		mkdir -p "$expected" && cp "$out/$build.json" "$expected/"
		verdict=recorded
	elif [ ! -e "$expected/$build.json" ]; then
		verdict='no expectation (run with --update)'
	elif diff -u "$expected/$build.json" "$out/$build.json" >"$out/$build.diff"; then
		verdict=ok
	else
		verdict=DIFFERS
	fi
	{
		printf '%-8s %-8s exit %-3s %s\n' "$version" "$build" "$status" "$verdict"
		[ "$verdict" != DIFFERS ] || sed 's/^/    /' "$out/$build.diff"
		[ "$status" -ne 70 ] || [ "$verdict" = ok ] || tail -n 5 "$out/$build.err" | sed 's/^/    /'
	} >"$out/$build.verdict"
}

failed=0
for guest in ${guests//,/ }; do
	[ "$guests" = "$guest" ] || echo "== $guest"
	if [ "$(uname)" = Darwin ]; then
		make -s -C examples "bin/$guest" || exit
	elif [ ! -x "examples/bin/$guest" ]; then
		echo "examples/bin/$guest is not there, and is built on a Mac" >&2
		exit 1
	fi
	out=$results/$guest expected=examples/expected/$guest args=() options=()
	mkdir -p "$out" || exit
	# objcinfo describes the frameworks it is given: one that only exists as a private framework.
	[ "$guest" != objcinfo ] || args=(/System/Library/PrivateFrameworks/IMCore.framework/IMCore)
	# service is run as the process that provides the service it then uses.
	[ "$guest" != service ] || options=(--service com.example.ipswrun.echo)
	# client is run beside the server whose services it uses, and prefs beside the release's own
	# cfprefsd, in both of its roles.
	[ "$guest" != client ] || options=(--agent com.example.ipswrun.echo,com.example.ipswrun.reverse=examples/bin/server)
	# adi is run beside the release's adid, the daemon it asks.
	[ "$guest" != adi ] || options=(--daemon com.apple.adid=system:/System/Library/PrivateFrameworks/CoreADI.framework/Versions/A/adid)
	[ "$guest" != prefs ] || options=(--agent 'com.apple.cfprefsd.agent=system:/usr/sbin/cfprefsd agent' --daemon 'com.apple.cfprefsd.daemon=system:/usr/sbin/cfprefsd daemon')

	# At most $jobs at once: when that many are running, wait for the oldest.
	pids=()
	for release in ${releases[@]+"${releases[@]}"}; do
		if [ ${#pids[@]} -ge "$jobs" ]; then
			wait "${pids[0]}"
			pids=("${pids[@]:1}")
		fi
		run_one $release &
		pids+=($!)
	done
	wait

	for release in ${releases[@]+"${releases[@]}"}; do
		cat "$out/${release#* }.verdict"
	done
	! cat "$out"/*.verdict 2>/dev/null | grep -q 'DIFFERS$\|no expectation' || failed=1
done
[ ${#skipped[@]} -eq 0 ] || printf '%s\n' "${skipped[@]}"

echo "results in $results"
exit $failed
