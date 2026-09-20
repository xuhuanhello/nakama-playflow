# Credentials and security

This repository contains public local-development credentials and reproducible test fixtures only. They are intentionally unusable as production configuration. Supply real PlayFlow keys, database credentials, signing keys and admin tokens through your server's environment or secret manager.

Do not put credentials in source code, Unity scenes/assets, command-line arguments, issue reports or logs. Keep production environment files, private keys and service-account files outside the repository. The ignore rules cover common filenames; they cannot identify every possible credential file.

The Secret scan workflow runs Gitleaks over Git history on pushes and pull requests. Its exceptions match only the exact documented dummy value in the exact fixture path; an entire directory or secret rule is never exempted. Check any new finding before adding an exception.

If a real credential is committed or published, revoke/rotate it first. Deleting the current file does not remove it from Git history, caches or existing clones. Do not post the credential in a public issue while reporting the problem.

The implementation's production integration limits are documented in README.md. Protocol and mock tests do not establish that an application's transport authentication, result persistence or cloud deployment is production-ready.
