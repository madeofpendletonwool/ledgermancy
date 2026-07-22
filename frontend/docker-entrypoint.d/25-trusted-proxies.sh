#!/bin/sh
# Expands $TRUSTED_PROXIES into nginx realip directives at container start.
#
# nginx cannot build a directive list from an environment variable, and the
# addresses in front of this container differ per deployment, so the snippet is
# generated here instead of baked into the image. nginx.conf includes the
# output unconditionally; when TRUSTED_PROXIES is unset the file contains only
# comments, which leaves nginx behaving exactly as it does with no realip
# configuration at all.
#
# The stock nginx entrypoint runs every executable /docker-entrypoint.d/*.sh
# before starting nginx, so this always runs first.
#
# See frontend/nginx.conf for why trusting these headers is safe only from
# addresses named here, and .env.example for the operator-facing description.

set -eu

out=/etc/nginx/trusted-proxies.conf

echo "# Generated from \$TRUSTED_PROXIES at container start. Do not edit --" > "$out"
echo "# every restart overwrites this file." >> "$out"

# Accept commas, spaces, tabs or newlines as separators so the value can be
# written whichever way reads best in an .env file or compose file.
# ${TRUSTED_PROXIES:-} rather than $TRUSTED_PROXIES because `set -u` is on.
proxies=$(printf '%s' "${TRUSTED_PROXIES:-}" | tr ',\t\n' '   ')

count=0
# Unquoted on purpose: word splitting is how the list is parsed.
for proxy in $proxies; do
    # Refuse anything that is not plausibly an address or CIDR range, rather
    # than writing it out and hoping. Two reasons this exits instead of
    # skipping: a value goes straight into a config file, so a stray ';' would
    # otherwise inject directives; and silently dropping a typo'd address is
    # indistinguishable from not configuring one, which is the single-shared-
    # bucket failure this whole mechanism exists to prevent. A container that
    # refuses to start with a clear reason is easier to diagnose than rate
    # limits that quietly stopped being per-client.
    case "$proxy" in
        *[!0-9a-fA-F.:/]*)
            echo "$0: invalid TRUSTED_PROXIES entry: '$proxy'" >&2
            echo "$0: expected IPv4/IPv6 addresses or CIDR ranges," \
                 "separated by commas or spaces (e.g. '10.0.0.4,172.18.0.0/16')" >&2
            exit 1
            ;;
    esac

    printf 'set_real_ip_from %s;\n' "$proxy" >> "$out"
    count=$((count + 1))
done

if [ "$count" -eq 0 ]; then
    {
        echo "#"
        echo "# TRUSTED_PROXIES is empty, so no proxy is trusted and \$remote_addr"
        echo "# stays the address that actually opened the connection."
    } >> "$out"
    echo "$0: TRUSTED_PROXIES not set; keeping the connecting address as the client IP"
    exit 0
fi

{
    # Only consulted for connections from the addresses above; anything else
    # keeps its real peer address regardless of what headers it sends.
    echo "real_ip_header X-Forwarded-For;"
    # Walk the header right-to-left, skipping addresses that are themselves
    # trusted, and stop at the first that is not. That is what makes a chain of
    # proxies resolve to the real client -- and what makes a value forged by
    # the client stop the walk at the forger's own address rather than be
    # believed.
    echo "real_ip_recursive on;"
} >> "$out"

echo "$0: trusting X-Forwarded-For from $count proxy address(es)"
