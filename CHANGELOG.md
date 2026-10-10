# Changelog

The section of each version is also what the app's update window shows, so
every release needs one here (`## 0.2.1`), written for the people updating.

## 0.2.1

- **Automatic updates.** MyToken checks github.com once a day and offers new
  versions with their notes; install, skip or be reminded later. Settings
  turns the check off, and has a button to check now. Updates are signed, and
  from a recent version only the changed parts are downloaded.
- The sidebar's "Up to date" now reads "Data up to date": it was always about
  the scan of your logs, not the app's version.

## 0.2.0

Relay reconciliation: see what your relay charged next to what your own logs
say it should have, and where the difference comes from.

- **Relay reconciliation** for new-api and sub2api relays, plus balances for
  some official APIs, off until you turn it on site by site: ratios become
  dated price rules, balances, and charges (new-api per request, sub2api per
  day) matched to your local requests.
- **Reconciliation page**: local estimate vs. charged, the implied multiplier,
  and a waterfall of where the difference comes from; by model and by day.
- **Tray** shows the balances of the sites you turned on.
- **CLI:** `mytoken relay list|enable|disable|sync` and `mytoken reconcile`.
- **12 more tools** (22 in total).
- **macOS:** the window buttons no longer overlap the sidebar.

First launch upgrades the database one way and reprices every request once.

## 0.1.3

See the [release](https://github.com/zzstar101/mytoken/releases/tag/v0.1.3).
