# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem. Report it
privately through GitHub's vulnerability reporting form:

https://github.com/ChristopherDavenport/agentsession/security/advisories/new

Include the affected version, a description of the issue and, if you
have one, a minimal reproduction. You will get an acknowledgement within
a week. Once a fix is available it ships as a new patch version and the
advisory is published with credit to the reporter, unless you prefer to
stay anonymous.

## Supported versions

Only the latest release receives security fixes.

## Scope

This library parses session files and ATIF documents that may come
from other harnesses or other machines, and it writes session files
that hold whole conversations, tool arguments and tool output. Anything
that lets a crafted file crash a process, exhaust its memory, escape the
store's root directory through a session ID or path, or make redaction
miss data it was asked to remove is in scope. Session files are written
with owner-only permissions; redaction runs only at export and only
over what the redactors are told to look for.
