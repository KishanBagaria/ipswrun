#!/bin/bash
# Runs a guest against every release that "all" stands for (ipswrun --releases) and diffs what each run reported
# against what is recorded for it in examples/expected/<guest>/<build>.json.
#
#   examples/matrix.sh [--guest=NAME,...] [--device=PRODUCT] [--jobs=N] [--stream-compressed] [--update] [version prefix...]
#
#   --guest=NAME,...     of GUESTS in examples/Makefile, instead of guest; all for every one
#   --device=PRODUCT     an iPhone or iPad (iPhone11,2) to run the iOS guests of examples/bin/ios
#                        against its releases, recorded in examples/expected/ios (default: macOS)
#   --jobs=N             runs at once, of any guest and release (default: one per processor)
#   --stream-compressed  also run macOS 13 and 14, or iOS 16 and 17, which download a 1.1–4.5 GB member each
#   --update             write the results to examples/expected instead of diffing against it
#   version prefix       only the builds whose version starts with one of these (e.g. 15. 27.2)
#
# Downloads are kept per build in $IPSWRUN_CACHE (default ~/Library/Caches/ipswrun/matrix), so a
# second run fetches nothing. Each run's report, output and diagnostics are left in $RESULTS/<guest>
# (default a temporary directory). Needs jq. Exits 1 if any build differs from what is expected.
# The guests are built on a Mac; anywhere else they have to be in examples/bin already.
set -u
cd "$(dirname "$0")/.."

guests=guest device= jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1) stream= update= prefixes=()
for arg; do
	case $arg in
	--guest=*) guests=${arg#--guest=} ;;
	--device=*) device=${arg#--device=} ;;
	--jobs=*) jobs=${arg#--jobs=} ;;
	--stream-compressed) stream=1 ;;
	--update) update=1 ;;
	-*) sed -n '2,18s/^# \{0,1\}//p' "$0"; exit 2 ;;
	*) prefixes+=("$arg") ;;
	esac
done

cache=${IPSWRUN_CACHE:-$HOME/Library/Caches/ipswrun/matrix}
results=${RESULTS:-$(mktemp -d "${TMPDIR:-/tmp}/ipswrun-matrix.XXXXXX")} || exit
mkdir -p "$results/bin" || exit
go build -o "$results/bin/ipswrun" ./cmd/ipswrun || exit
[ "$guests" != all ] || guests=$(sed -n 's/^GUESTS = //p' examples/Makefile)

# An iPhone or iPad runs iOS: its own guests, expectations and releases.
platform=macos bin=examples/bin expected_root=examples/expected device_flags=()
case $device in iPhone* | iPad* | iPod*) platform=ios bin=examples/bin/ios expected_root=examples/expected/ios ;; esac
[ -z "$device" ] || device_flags=(--device "$device")

# compressed says whether a version keeps its shared cache in a deflated member, which only
# --stream-compressed reads: macOS 13 and 14, iOS 16 and 17.
compressed() {
	case $platform:$1 in macos:13.* | macos:14.* | ios:16.* | ios:17.*) return 0 ;; esac
	return 1
}

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
	if compressed "$version" && [ -z "$stream" ]; then
		skipped+=("$(printf '%-8s %-8s skipped (needs --stream-compressed)' "$version" "$build")")
		continue
	fi
	releases+=("$version $build")
done < <("$results/bin/ipswrun" ${device_flags[@]+"${device_flags[@]}"} --releases)

# run_one runs $guest against one release and leaves its verdict, and what to show with it, in
# $out/$build.verdict. Each release has its own cache directory, which several runs can share.
run_one() {
	local version=$1 build=$2 flags=() verdict status
	! compressed "$version" || flags=(--stream-compressed)
	flags+=(${device_flags[@]+"${device_flags[@]}"})
	local home=$out/$build.home report=$out/$build.report.json
	rm -rf "$home" "$report"
	mkdir -p "$home" "$cache/$build"
	# ${flags[@]+…}: an empty array is "unbound" to the bash 3.2 that macOS ships.
	"$results/bin/ipswrun" ${flags[@]+"${flags[@]}"} ${options[@]+"${options[@]}"} --cache "$cache/$build" --home "$home" --report "$report" \
		"$build" "$bin/$guest" ${args[@]+"${args[@]}"} >"$out/$build.out" 2>"$out/$build.err" </dev/null
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

# Every guest's runs share the $jobs slots, so a guest with fewer releases than processors leaves
# none idle: at most $jobs at once, and when that many are running, wait for the oldest.
pids=()
for guest in ${guests//,/ }; do
	# adi asks the release's adid, which on iOS is another daemon, of FairPlay's, that needs more
	# of an iPhone than there is.
	[ "$platform" != ios ] || [ "$guest" != adi ] || continue
	# shell runs /bin/sh and /bin/echo, which iOS has not.
	[ "$platform" != ios ] || [ "$guest" != shell ] || continue
	if [ "$(uname)" = Darwin ]; then
		make -s -C examples PLATFORM=$platform "${bin#examples/}/$guest" || exit
	elif [ ! -x "$bin/$guest" ]; then
		echo "$bin/$guest is not there, and is built on a Mac" >&2
		exit 1
	fi
	out=$results/$guest expected=$expected_root/$guest args=() options=()
	mkdir -p "$out" || exit
	# objcinfo describes the frameworks it is given: one that only exists as a private framework.
	[ "$guest" != objcinfo ] || args=(/System/Library/PrivateFrameworks/IMCore.framework/IMCore)
	# service is run as the process that provides the service it then uses.
	[ "$guest" != service ] || options=(--service com.example.ipswrun.echo)
	# client is run beside the server whose services it uses, and prefs beside the release's own
	# cfprefsd, in both of its roles.
	[ "$guest" != client ] || options=(--agent com.example.ipswrun.echo,com.example.ipswrun.reverse=$bin/server)
	# adi is run beside the release's adid, the daemon it asks.
	[ "$guest" != adi ] || options=(--daemon com.apple.adid=system:/System/Library/PrivateFrameworks/CoreADI.framework/Versions/A/adid)
	# Two variables of ipswrun's keep CoreFoundation from asking for cfprefsd, and go. On macOS
	# cfprefsd is started when it is first looked up, in both of its roles, as its launchd plists
	# say; on iOS, where the agent has no plist of its own, both are started by hand. cfprefsd
	# reads its sandbox profile, which every guest may.
	[ "$guest" != prefs ] || options=(--env __CFPREFERENCES_AVOID_DAEMON --env CFFIXED_USER_HOME --on-demand 'com.apple.cfprefsd.*')
	[ "$guest" != prefs ] || [ "$platform" != ios ] || options=(--env __CFPREFERENCES_AVOID_DAEMON --env CFFIXED_USER_HOME
		--agent 'com.apple.cfprefsd.agent=system:/usr/sbin/cfprefsd agent' --daemon 'com.apple.cfprefsd.daemon=system:/usr/sbin/cfprefsd daemon')

	for release in ${releases[@]+"${releases[@]}"}; do
		if [ ${#pids[@]} -ge "$jobs" ]; then
			wait "${pids[0]}"
			pids=("${pids[@]:1}")
		fi
		run_one $release &
		pids+=($!)
	done
done
wait

failed=0
for guest in ${guests//,/ }; do
	if [ "$platform" = ios ] && { [ "$guest" = adi ] || [ "$guest" = shell ]; }; then
		echo "== $guest: not run on iOS"
		continue
	fi
	[ "$guests" = "$guest" ] || echo "== $guest"
	out=$results/$guest
	for release in ${releases[@]+"${releases[@]}"}; do
		cat "$out/${release#* }.verdict"
	done
	! cat "$out"/*.verdict 2>/dev/null | grep -q 'DIFFERS$\|no expectation' || failed=1
done
[ ${#skipped[@]} -eq 0 ] || printf '%s\n' "${skipped[@]}"

echo "results in $results"
exit $failed
