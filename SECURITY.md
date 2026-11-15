# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the repository's
**Security** tab and choose **Report a vulnerability**. Do not open a public
issue or pull request for a security problem.

Include the affected version or commit, how to reproduce it, and the impact
you expect. You should get a first answer within a week.

## Supported versions

Only the latest commit on `main` receives fixes.

## Deployment notes

- Always set API tokens when the service is reachable by anyone but you.
- Put the service behind HTTPS (a reverse proxy or the built-in TLS options,
  where available) when it leaves a private network.
- Never commit `.env`, `secrets.json` or token files; they are git-ignored.
