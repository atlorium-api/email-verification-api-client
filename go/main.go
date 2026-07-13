// Клиент API «Проверка почты» Atlorium — существует ли ящик, не спам-ловушка ли это.
//
// Запуск (работает сразу, без регистрации — на демо-ключе):
//
//	go run .
//	go run . "anna@example.com,promo@spamtrap.example.com"
//
// Боевой ключ: получить на https://atlorium.com и положить в переменную окружения
// ATLORIUM_API_KEY. Код при этом не меняется.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// SandboxKey — публичный демо-ключ. С ним API отвечает правдоподобными МОКАМИ
// (не реальными данными), чтобы можно было встроить интеграцию до оплаты.
// Ответы детерминированы — на них можно писать стабильные тесты.
const SandboxKey = "ak_sandbox_demo_mockdata_v1"

// Проверка почты — платный сервис, лимит жёсткий.
// Список рассылки длиннее двух адресов гарантированно упрётся в 429 — поэтому
// повтор после паузы здесь не «на всякий случай», а штатный режим работы.
//
// MaxRetryDelay — потолок ожидания. Исчерпав ЧАСОВОЙ лимит, сервер честно отвечает
// Retry-After на 40+ минут, и клиент, слепо доверяющий заголовку, «зависнет» на эти
// 40 минут (а в CI просто съест весь бюджет джоба). Дольше потолка не ждём.
const (
	RetryDelay    = 30 * time.Second
	MaxRetries    = 3
	MaxRetryDelay = 120 * time.Second
)

var (
	apiKey  = envOr("ATLORIUM_API_KEY", SandboxKey)
	baseURL = envOr("ATLORIUM_BASE_URL", "https://atlorium.com")

	// Проверка ящика — синхронное обращение к почтовому серверу домена, он может
	// тянуть с ответом. Отсюда таймаут заметно больше, чем у справочников.
	client = &http.Client{Timeout: 30 * time.Second}
)

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// EmailReport — карточка проверки одного адреса.
type EmailReport struct {
	Email string `json:"email"`

	// Итоговый вердикт: Valid | Invalid | CatchAll | Unknown | SpamTrap | Abuse | DoNotMail.
	Status string `json:"status"`

	// Статус ровно в том виде, как его вернул источник: "valid", "catch-all", "do_not_mail".
	StatusRaw string `json:"statusRaw"`

	// Уточнение: "mailbox_not_found", "disposable", "role_based", "greylisted".
	SubStatus *string `json:"subStatus"`

	Account        *string `json:"account"`
	Domain         *string `json:"domain"`
	FreeEmail      bool    `json:"freeEmail"`
	CatchAllDomain *bool   `json:"catchAllDomain"`

	// DidYouMean — подсказка по опечатке в домене: gmial.com → gmail.com.
	DidYouMean *string `json:"didYouMean"`

	DomainAgeDays   *int    `json:"domainAgeDays"`
	SmtpProvider    *string `json:"smtpProvider"`
	MxFound         bool    `json:"mxFound"`
	MxRecord        *string `json:"mxRecord"`
	ActiveInDays    *string `json:"activeInDays"`
	ActiveFirstSeen *string `json:"activeFirstSeen"`
	ProcessedAtUtc  *string `json:"processedAtUtc"`
	ElapsedMs       int64   `json:"elapsedMs"`

	// Deliverable — true только при Status = Valid. catch-all и unknown — неопределённость.
	Deliverable bool `json:"deliverable"`
}

// APIError раскладывает HTTP-код в человекочитаемую причину.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	reasons := map[int]string{
		400: "адрес не передан или синтаксически некорректен (запрос НЕ тарифицируется)",
		401: "API-ключ отсутствует, просрочен или недействителен",
		402: "недостаточно кредитов на балансе — пополните на https://atlorium.com",
		429: "превышен лимит запросов — повторите позже",
		503: "сервис проверки почты временно недоступен (за сбой на своей стороне мы не списываем деньги)",
	}
	reason, ok := reasons[e.Status]
	if !ok {
		reason = "неизвестная ошибка"
	}
	return fmt.Sprintf("HTTP %d: %s. Ответ сервера: %s", e.Status, reason, e.Body)
}

// retryAfter сообщает, сколько ждать после 429. Ноль/мусор и слишком большие
// значения не берём на веру.
//
// Значение 0 (или мусор) означало бы «повторяй немедленно» — клиент ушёл бы в
// busy-loop и выжег остаток лимита за секунду. Значение в 40+ минут (так сервер
// отвечает на исчерпанный часовой лимит) означало бы «спи почти час» — этого мы
// тоже не делаем. Возвращаем 0, если ждать бессмысленно долго: вызывающий сдастся.
func retryAfter(response *http.Response) time.Duration {
	seconds, err := strconv.Atoi(response.Header.Get("Retry-After"))
	if err != nil || seconds <= 0 {
		return RetryDelay
	}
	if delay := time.Duration(seconds) * time.Second; delay <= MaxRetryDelay {
		return delay
	}
	return 0
}

// VerifyEmail проверяет один адрес: GET /api/EmailValidation?email=...
//
// Письмо на адрес НЕ отправляется: провайдер обращается к почтовому серверу домена
// и выясняет, принял бы тот письмо на этот ящик.
func VerifyEmail(email string) (*EmailReport, error) {
	endpoint := baseURL + "/api/EmailValidation?" + url.Values{"email": {email}}.Encode()

	for attempt := 0; ; attempt++ {
		request, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+apiKey)
		request.Header.Set("Accept", "application/json")

		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}

		// 429 — не поломка, а реальный лимит продукта. Ждём и повторяем.
		if response.StatusCode == http.StatusTooManyRequests && attempt < MaxRetries {
			delay := retryAfter(response)
			response.Body.Close()
			if delay == 0 {
				// Сервер просит ждать дольше потолка — значит, исчерпан часовой лимит.
				// Спать 40 минут бессмысленно: честно говорим об этом и выходим.
				return nil, &APIError{Status: http.StatusTooManyRequests,
					Body: "лимит запросов по IP исчерпан, повторите позже"}
			}
			fmt.Fprintf(os.Stderr, "  ... лимит запросов, пауза %.0f с\n", delay.Seconds())
			time.Sleep(delay)
			continue
		}

		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			return nil, &APIError{Status: response.StatusCode, Body: string(body)}
		}

		var report EmailReport
		if err := json.Unmarshal(body, &report); err != nil {
			return nil, err
		}
		return &report, nil
	}
}

// ── Применение данных: чистка списка рассылки ─────────────────────────────────
// Отчёт по адресу сам по себе — просто JSON. Ценность появляется, когда по нему
// принимают решение: слать письмо или нет. Ниже — ровно это решение.

// Решения по адресу.
const (
	ActionSend       = "SEND"        // отправляем
	ActionSendRisky  = "SEND_RISKY"  // можно отправить, но на свой риск
	ActionRetryLater = "RETRY_LATER" // проверить не удалось, повторить позже
	ActionDrop       = "DROP"        // из списка выбросить
)

// Decision — что делать с адресом.
type Decision struct {
	Email     string
	StatusRaw string
	Action    string
	Reason    string

	// SpamTrap вынесен в отдельный флаг: это не «ещё один DROP», а причина
	// остановить рассылку и разобраться, откуда взялся адрес.
	SpamTrap bool

	// DidYouMean — подсказка по опечатке в домене: gmial.com → gmail.com.
	DidYouMean string
}

// Decide решает, что делать с адресом, исходя из вердикта проверки.
func Decide(report *EmailReport) Decision {
	decision := Decision{Email: report.Email, StatusRaw: report.StatusRaw}
	if report.DidYouMean != nil {
		decision.DidYouMean = *report.DidYouMean
	}

	subStatus := ""
	if report.SubStatus != nil {
		subStatus = *report.SubStatus
	}

	switch report.Status {
	case "Valid":
		decision.Action = ActionSend
		decision.Reason = "Ящик существует, домен принимает почту"

	case "CatchAll":
		// Домен отвечает «принято» на ЛЮБОЙ адрес, поэтому существование конкретного
		// ящика проверить снаружи невозможно. Это не «плохо» — это неопределённость.
		// Слать можно, но отдельным сегментом и следя за bounce rate.
		decision.Action = ActionSendRisky
		decision.Reason = "Домен принимает всё подряд — доставляемость не гарантирована"

	case "Unknown":
		// Сервер домена применил greylisting, не ответил или заблокировал проверку.
		// За такой исход деньги НЕ списываются — адрес просто проверяем позже.
		decision.Action = ActionRetryLater
		decision.Reason = "Сервер домена не ответил (greylisting) — запрос не тарифицирован"

	case "Invalid":
		// Ящика не существует. Отправка = hard bounce, а bounce rate выше 2 %
		// почтовые провайдеры считают признаком грязной базы.
		decision.Action = ActionDrop
		decision.Reason = "Ящика не существует — письмо отскочит (hard bounce)"

	case "DoNotMail":
		// Технически ящик может работать, но рассылать на него нельзя.
		decision.Action = ActionDrop
		switch subStatus {
		case "role_based":
			decision.Reason = "Ролевой адрес (info@, sales@) — читает не человек, а отдел"
		case "disposable":
			decision.Reason = "Одноразовый ящик — создан на 10 минут, писать бессмысленно"
		default:
			decision.Reason = "Адрес из списка «не писать»"
		}

	case "Abuse":
		// Владелец адреса уже жаловался на спам. Ещё одна жалоба — минус к репутации.
		decision.Action = ActionDrop
		decision.Reason = "Жалобщик: ранее отмечал письма как спам"

	case "SpamTrap":
		// САМОЕ ВАЖНОЕ, ЧТО ДАЁТ СЕРВИС.
		// Спам-ловушка — адрес, который никто не заводил и на который никто не
		// подписывался: его специально публикуют, чтобы поймать тех, кто рассылает
		// по купленным и спарсенным базам. Ящик отвечает как живой, поэтому отличить
		// его самому невозможно. Одно попадание — и репутация домена-отправителя
		// падает, а в спам уходит ВСЯ рассылка, включая письма живым клиентам.
		decision.Action = ActionDrop
		decision.Reason = "СПАМ-ЛОВУШКА — убивает репутацию домена-отправителя"
		decision.SpamTrap = true

	default:
		// Провайдер вернул статус, которого мы не знаем. Не рискуем.
		decision.Action = ActionDrop
		decision.Reason = "Неизвестный статус: " + report.StatusRaw
	}

	return decision
}

// Summary — результат чистки списка.
type Summary struct {
	Decisions []Decision
}

// Count считает адреса с указанным решением.
func (s Summary) Count(action string) int {
	total := 0
	for _, d := range s.Decisions {
		if d.Action == action {
			total++
		}
	}
	return total
}

// Invalid — сколько ящиков не существует.
func (s Summary) Invalid() int {
	total := 0
	for _, d := range s.Decisions {
		if d.StatusRaw == "invalid" {
			total++
		}
	}
	return total
}

// SpamTraps — попавшие в список спам-ловушки.
func (s Summary) SpamTraps() []Decision {
	var found []Decision
	for _, d := range s.Decisions {
		if d.SpamTrap {
			found = append(found, d)
		}
	}
	return found
}

// Typos — адреса с подсказкой по опечатке.
func (s Summary) Typos() []Decision {
	var found []Decision
	for _, d := range s.Decisions {
		if d.DidYouMean != "" {
			found = append(found, d)
		}
	}
	return found
}

// BounceRate — какой был бы bounce rate, если бы список отправили как есть.
func (s Summary) BounceRate() float64 {
	if len(s.Decisions) == 0 {
		return 0
	}
	return 100 * float64(s.Invalid()) / float64(len(s.Decisions))
}

// FilterMailingList чистит список рассылки: каждый адрес проверяется и получает решение.
func FilterMailingList(emails []string) (Summary, error) {
	var summary Summary
	for _, email := range emails {
		report, err := VerifyEmail(email)
		if err != nil {
			return summary, err
		}
		summary.Decisions = append(summary.Decisions, Decide(report))
	}
	return summary, nil
}

// pad дополняет строку пробелами до нужной ширины (в рунах, а не в байтах —
// иначе кириллица разъедет колонки).
func pad(value string, width int) string {
	if runes := len([]rune(value)); runes < width {
		return value + strings.Repeat(" ", width-runes)
	}
	return value
}

func main() {
	if apiKey == SandboxKey {
		fmt.Println("Демо-ключ: ответы сгенерированы (моки), не реальные данные.")
		fmt.Println()
	}

	// Список короткий намеренно: проверка почты — платный сервис с жёстким лимитом.
	// Домены ниже — сценарии песочницы, см. README.
	emails := []string{
		"anna.petrova@example.com",   // обычный живой ящик
		"ivan@invalid.example.com",   // ящика не существует
		"promo@spamtrap.example.com", // спам-ловушка
		"pavel@gmial.com",            // опечатка в домене: gmial → gmail
	}

	if len(os.Args) > 1 {
		emails = nil
		for _, part := range strings.Split(os.Args[1], ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				emails = append(emails, trimmed)
			}
		}
	}

	fmt.Printf("Чистка списка рассылки. Адресов на входе: %d\n\n", len(emails))

	summary, err := FilterMailingList(emails)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		os.Exit(1)
	}

	fmt.Printf("%s%s%s%s\n", pad("АДРЕС", 32), pad("СТАТУС", 11), pad("РЕШЕНИЕ", 13), "КОММЕНТАРИЙ")
	for _, d := range summary.Decisions {
		fmt.Printf("%s%s%s%s\n", pad(d.Email, 32), pad(d.StatusRaw, 11), pad(d.Action, 13), d.Reason)
	}

	if typos := summary.Typos(); len(typos) > 0 {
		fmt.Println("\nПОДСКАЗКИ ПО ОПЕЧАТКАМ (это спасённый контакт, а не потерянный):")
		for _, d := range typos {
			fmt.Printf("  [~] возможно, опечатка: %s → %s\n", d.Email, d.DidYouMean)
		}
	}

	spamtraps := summary.SpamTraps()
	if len(spamtraps) > 0 {
		fmt.Printf("\n!!! СПАМ-ЛОВУШКА В СПИСКЕ: %d !!!\n", len(spamtraps))
		for _, d := range spamtraps {
			fmt.Printf("  %s\n", d.Email)
		}
		fmt.Println("  Такой адрес никто не заводил и ни на что не подписывал — его публикуют,")
		fmt.Println("  чтобы ловить рассылки по купленным базам. Одно попадание роняет репутацию")
		fmt.Println("  домена-отправителя, после чего в спам уходит ВСЯ рассылка, включая письма")
		fmt.Println("  живым клиентам. Удалять безусловно и выяснять, откуда адрес взялся.")
	}

	fmt.Println("\nИТОГО")
	fmt.Printf("  Проверено адресов:      %d\n", len(summary.Decisions))
	fmt.Printf("  К отправке (SEND):      %d\n", summary.Count(ActionSend))
	fmt.Printf("  На свой риск (RISKY):   %d\n", summary.Count(ActionSendRisky))
	fmt.Printf("  Повторить позже:        %d  (не тарифицируется)\n", summary.Count(ActionRetryLater))
	fmt.Printf("  Выброшено (DROP):       %d\n", summary.Count(ActionDrop))
	fmt.Printf("  Bounce rate без чистки: %.1f %%  (несуществующих ящиков: %d из %d)\n",
		summary.BounceRate(), summary.Invalid(), len(summary.Decisions))
	if len(spamtraps) > 0 {
		fmt.Printf("  Спам-ловушек:           %d  <-- критично\n", len(spamtraps))
	}
}
