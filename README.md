# Payback

A small Go program, shipped as a Docker image, that takes your Plex watch history and works out what it would have cost on streaming services if you'd watched the cheapest way, so you can see how much of your NAS has paid for itself.

![Payback, showing demo data](docs/screenshot.png)

## How it prices

- **Shows:** one 30-day subscription to the cheapest service that has the show, renewed only while you keep watching.
- **Movies:** free if a service that has it is already running, otherwise the cheaper of a rental or a month of a service that has it.
- **Shows no service has:** bought by the episode, capped at the season price.

Where things stream comes from TMDB (data by JustWatch). Costs and prices can be edited on the page.

## Run it

1. Get a [Plex token](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/) and a free [TMDB API Read Access Token](https://www.themoviedb.org/settings/api).
2. Put them in [`compose.yaml`](compose.yaml) along with your Plex address, then run `docker compose up -d`.
3. Open `http://<server>:8095` and enter what your setup cost under **Costs**.

Want to see it first? `docker run --rm -p 8095:8095 -e DEMO=1 ghcr.io/rmahshie/plex-payback`

## Settings

| Variable | |
| --- | --- |
| `PLEX_URL` | Your Plex server, e.g. `http://192.168.1.10:32400` |
| `PLEX_TOKEN` | Plex token |
| `TMDB_TOKEN` | TMDB API Read Access Token |
| `REGION` | Country for streaming availability and default prices (default `US`) |
| `TZ` | Time zone for counting months, e.g. `America/Chicago` |
| `APP_PASSWORD` | Optional password for the page |

Data is kept in `/data`. Your history stays there: TMDB only gets title lookups.

## Credits

Data from [TMDB](https://www.themoviedb.org) and [JustWatch](https://www.justwatch.com). This product uses the TMDB API but is not endorsed or certified by TMDB. Not affiliated with Plex. Font: Barlow Condensed ([OFL](web/fonts/OFL.txt)).

MIT licensed.
