#!/bin/sh
set -eu

if [ "$#" -lt 2 ]; then
  echo "usage: run-with-timeout.sh <seconds> <command> [arguments...]" >&2
  exit 2
fi

limit=$1
shift

if command -v gtimeout >/dev/null 2>&1; then
  exec gtimeout --kill-after=2s "${limit}s" "$@"
fi
if command -v timeout >/dev/null 2>&1; then
  exec timeout --kill-after=2s "${limit}s" "$@"
fi

exec perl -e '
  use strict;
  use warnings;
  use POSIX qw(setpgid);
  my $limit = shift @ARGV;
  my $pid = fork();
  die "fork failed: $!" unless defined $pid;
  if ($pid == 0) {
    setpgid(0, 0);
    exec @ARGV;
    exit 127;
  }
  $SIG{ALRM} = sub {
    system "/bin/kill", "-TERM", "--", "-$pid";
    select undef, undef, undef, 2;
    system "/bin/kill", "-KILL", "--", "-$pid";
    waitpid($pid, 0);
    exit 124;
  };
  alarm $limit;
  waitpid($pid, 0);
  alarm 0;
  exit($? == -1 ? 127 : $? >> 8);
' "$limit" "$@"
