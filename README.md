My personal fork of mautrix-discord with some potentially unstable patches.

## Additions
- **Discord<->Matrix**: `discord.com/channels/` and `matrix.to` links are now translated to each other.
- **Matrix->Discord**: Webhook replies are now prettier (stolen from [Out Of Your Element](https://gitdab.com/cadence/out-of-your-element) &lt;3).

## Fixes
- **Discord->Matrix**: Read receipts are now private instead of public.
- **Discord->Matrix**: Mentioning a role with the default role color now shows it as white instead of black.
- **Discord->Matrix**: When a message is forwarded on Discord, its attachments are now shown on Matrix.
- **Matrix->Discord**: Replying to an edited message now shows the latest rather than original content.
