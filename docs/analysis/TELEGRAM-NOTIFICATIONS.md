# Telegram Notifications Integration Analysis

> **Status:** Analysis complete, not implemented
> **Date:** 2026-04-01

## Cosa notificare

| Evento | Quando | Priorità |
|--------|--------|----------|
| **Limiti attivati** | `makeDecision()` → ACTIVATE_LIMITS | Alta |
| **Limiti disattivati** | `makeDecision()` → DEACTIVATE_LIMITS | Info |
| **Soglia superata (warning)** | Risorsa sopra soglia ma duration non ancora scaduta | Media |
| **Sistema sotto carico** | `SystemUnderLoad=true` + limiti non attivabili | Alta |
| **Utente specifico limitato** | Singolo utente supera soglia individuale | Media |

## Configurazione proposta

```bash
# Telegram Notifications
TELEGRAM_ENABLED=false
TELEGRAM_BOT_TOKEN=
TELEGRAM_CHAT_ID=
TELEGRAM_NOTIFY_ACTIVATION=true      # Notifica quando si attivano limiti
TELEGRAM_NOTIFY_DEACTIVATION=true    # Notifica quando si rilasciano limiti
TELEGRAM_NOTIFY_WARNING=true         # Notifica warning (soglia superata, duration non scaduta)
TELEGRAM_RATE_LIMIT=300              # Minimo secondi tra notifiche (default 5 min)
TELEGRAM_TIMEOUT=10                  # Timeout HTTP request (secondi)
```

## Architettura

```
notify/
  notifier.go       # Interfaccia Notifier + dispatcher
  telegram.go       # Implementazione Telegram Bot API
  config.go         # Config struct per notifiche

state/manager.go
  └── controlCycle()
        └── makeDecision() → se ACTIVATE/DEACTIVATE → notifyDispatcher.Send()
```

## Design dell'interfaccia

```go
type Notifier interface {
    Send(ctx context.Context, msg Notification) error
}

type Notification struct {
    Level    Level    // info, warning, critical
    Title    string
    Message  string
    Details  map[string]string
    Time     time.Time
}
```

## Dove integrare nel control cycle

```go
// In state/manager.go, dopo executeDecision():
if decision == DecisionActivate {
    n := notify.Notification{
        Level: notify.Critical,
        Title: "Limits Activated",
        Message: fmt.Sprintf("CPU: %.1f%%, RAM: %d%%, IO: %d%%", ...),
    }
    m.notifier.Send(context.Background(), n)
}
```

## Fattori nascosti da considerare

1. **Rate limiting**: Se il sistema oscilla intorno alla soglia, non vuoi 100 notifiche/ora. Serve un cooldown configurabile.

2. **Blackout respect**: Le notifiche devono rispettare `CPU_MANAGER_BLACKOUT` — non svegliare alle 3 di notte se è in blackout.

3. **Fallback silenzioso**: Se Telegram è down o il token è invalido, resman non deve crashare o bloccare il control cycle.

4. **Privacy**: Il messaggio include username e UID — su server multi-tenant potrebbe essere sensibile.

5. **Multi-channel**: Progettare l'interfaccia in modo che in futuro si possano aggiungere Slack, email, webhook senza riscrivere la logica.

6. **Aggregazione**: Se 10 utenti vengono limitati contemporaneamente, meglio un messaggio "10 utenti limitati" che 10 messaggi separati.

## Stima effort

| Componente | Righe | Complessità |
|------------|-------|-------------|
| `notify/notifier.go` (interfaccia + dispatcher) | ~80 | Bassa |
| `notify/telegram.go` (Telegram Bot API) | ~120 | Media |
| `config/config.go` (campi + validazione) | ~30 | Bassa |
| `state/manager.go` (integrazione) | ~40 | Bassa |
| Test + mock | ~100 | Media |
| Docs + config example | ~30 | Bassa |
| **Totale** | **~400 righe** | **~3-4 ore** |
