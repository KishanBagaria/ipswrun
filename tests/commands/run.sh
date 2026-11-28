#!/bin/bash
# Runs lists of shell commands under ipswrun, by the release's own /bin/sh, and holds what each
# prints against what a Mac printed for it.
#
#   tests/commands/run.sh [--record] [--jobs=N] [list...]
#
#   list        of tests/commands/*.list, by name (default: every one). Each line is a command,
#               run in a new directory that has a.txt ("hello"), a copy of it c.txt and a
#               directory d/e, with what it prints and what it says of errors taken together,
#               the first 3000 bytes of it.
#   --record    run the commands on this Mac instead, and keep what they print as what is
#               expected (tests/commands/expected/<list>/<hash of the command>.txt). Only on a
#               Mac that runs the release named in tests/commands/release.
#   --jobs=N    commands at once (default: one per processor)
#
# What a Mac printed is right only for the release it ran, so the commands are run against that
# one release: tests/commands/release names it, by version and build. A command that is known
# not to print what the Mac printed is listed in tests/commands/known.tsv with how it ends
# (differs, or stops: ipswrun says what it does not implement) and why; any other that does not
# print the same, and any listed one that now does or ends another way, fails the run.
#
# Downloads are kept in $IPSWRUN_CACHE (default ~/Library/Caches/ipswrun/matrix), as
# tests/matrix.sh keeps them. Results are left in $RESULTS (default a temporary directory).
set -u
cd "$(dirname "$0")/../.."
here=tests/commands

record= jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1) lists=()
for arg; do
	case $arg in
	--record) record=1 ;;
	--jobs=*) jobs=${arg#--jobs=} ;;
	-*) sed -n '2,24s/^# \{0,1\}//p' "$0"; exit 2 ;;
	*) lists+=("$arg") ;;
	esac
done
[ ${#lists[@]} -gt 0 ] || for f in $here/*.list; do lists+=("$(basename "$f" .list)"); done
read -r version build < $here/release || exit

cache=${IPSWRUN_CACHE:-$HOME/Library/Caches/ipswrun/matrix}
results=${RESULTS:-$(mktemp -d "${TMPDIR:-/tmp}/ipswrun-commands.XXXXXX")} || exit
mkdir -p "$results/bin" "$results/work" || exit

# What every command starts with: the tools of the system and nothing of the host's, the one
# time zone whatever the Mac it was recorded on is set to, and the files the lists count on.
setup='export PATH=/usr/bin:/bin:/usr/sbin:/sbin TZ=UTC; echo hello > a.txt; mkdir -p d/e; cp a.txt c.txt; '

name() { printf '%s' "$1" | shasum -a 256 | cut -c1-16; }

if [ -n "$record" ]; then
	[ "$(sw_vers -buildVersion 2>/dev/null)" = "$build" ] || { echo "this is not a Mac running $version ($build): what it prints would not be that release's" >&2; exit 1; }
else
	go build -o "$results/bin/ipswrun" ./cmd/ipswrun || exit
fi

# one runs a command of a list, natively or under ipswrun, and leaves what it printed in
# $results/<list>/<hash>.txt.
one() {
	local list=$1 line=$2 work out
	out=$results/$list/$(name "$line").txt
	work=$(mktemp -d "$results/work/XXXXXX") || return
	if [ -n "$record" ]; then
		(cd "$work" && perl -e 'alarm 20; exec @ARGV' /bin/sh -c "$setup$line" < /dev/null 2>&1 | head -c 3000) > "$out"
	else
		mkdir -p "$work/w" "$work/tmp"
		"$results/bin/ipswrun" --timeout 60 --cache "$cache/$build" --home "$work" "$build" system:/bin/sh -c "cd \$HOME/w; $setup$line" < /dev/null 2>&1 | head -c 3000 > "$out"
	fi
}

status=0
for list in "${lists[@]}"; do
	[ -f "$here/$list.list" ] || { echo "no list $list" >&2; exit 2; }
	mkdir -p "$results/$list"
	running=0
	while IFS= read -r line; do
		case $line in \#* | "") continue ;; esac
		one "$list" "$line" &
		running=$((running + 1))
		[ $running -lt "$jobs" ] || { wait; running=0; } # so many at once, and then the next so many
	done < "$here/$list.list"
	wait

	if [ -n "$record" ]; then
		mkdir -p "$here/expected/$list"
		find "$here/expected/$list" -name '*.txt' -delete
		cp "$results/$list"/*.txt "$here/expected/$list/"
		echo "$list: recorded $(ls "$here/expected/$list" | wc -l | tr -d ' ') commands on $version ($build)"
		continue
	fi

	same=0 wrong=0 known=0
	while IFS= read -r line; do
		case $line in \#* | "") continue ;; esac
		h=$(name "$line") ended=same
		if [ ! -f "$here/expected/$list/$h.txt" ]; then
			ended=unrecorded
		elif ! cmp -s "$results/$list/$h.txt" "$here/expected/$list/$h.txt"; then
			ended=differs
			! grep -q '^ipswrun: ' "$results/$list/$h.txt" || ended=stops
		fi
		expected=$(awk -F'\t' -v l="$list" -v c="$line" '$1 == l && $4 == c { print $2 }' $here/known.tsv)
		if [ "$ended" = "${expected:-same}" ]; then
			[ "$ended" = same ] && same=$((same + 1)) || known=$((known + 1))
			continue
		fi
		wrong=$((wrong + 1)) status=1
		echo "$list: ${ended}, where ${expected:-same} was expected: $line"
		if [ "$ended" != same ] && [ "$ended" != unrecorded ]; then
			diff "$here/expected/$list/$h.txt" "$results/$list/$h.txt" | head -6 | cut -c1-200 | sed 's/^/    /'
		fi
	done < "$here/$list.list"
	echo "$list on $version ($build): $same print what a Mac prints, $known do not and are known, $wrong are not as expected"
done
echo "results in $results"
exit $status
