// Package i18n answers in Spanish to a browser that prefers it and in English to every other, as the
// page does. Texts are keyed by their English, a fmt format when something is filled in.
package i18n

import (
	"fmt"
	"net/http"
	"strings"
)

// Spanish is true when the browser's first language is any Spanish.
func Spanish(r *http.Request) bool {
	first, _, _ := strings.Cut(r.Header.Get("Accept-Language"), ",")
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(first)), "es")
}

// Lang is the page's lang attribute for r.
func Lang(r *http.Request) string {
	if Spanish(r) {
		return "es"
	}
	return "en"
}

func T(r *http.Request, text string, args ...any) string {
	if s, ok := es[text]; ok && Spanish(r) {
		text = s
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

var es = map[string]string{
	// Requests the page never sends
	"Use GET":                                     "Usa GET",
	"Use POST, PATCH or DELETE":                   "Usa POST, PATCH o DELETE",
	"Send the change as JSON":                     "Envía el cambio como JSON",
	"Send the source as JSON":                     "Envía la fuente como JSON",
	"That request came from another site":         "Esa petición venía de otro sitio",
	"The change could not be read":                "No se pudo leer el cambio",
	"The source could not be read":                "No se pudo leer la fuente",
	"The role is admin or viewer":                 "El rol es administrador o lector",
	"Sharing is not set up on this bifrost":       "Compartir no está configurado en este bifrost",
	"Invite links are not set up on this bifrost": "Los enlaces de invitación no están configurados en este bifrost",

	// Sharing
	"Only an admin of this bifrost can change who has access":    "Solo un administrador de este bifrost puede cambiar quién tiene acceso",
	"Not an email address: %s":                                   "No es una dirección de correo: %s",
	"Give at least one email address":                            "Indica al menos una dirección de correo",
	"You cannot remove yourself: ask another admin":              "No puedes quitarte a ti mismo: pídeselo a otro administrador",
	"That admin is set in bifrost's settings, not from the page": "Ese administrador está definido en la configuración de bifrost, no desde la página",
	"You cannot change your own role: ask another admin":         "No puedes cambiar tu propio rol: pídeselo a otro administrador",
	"Only someone on this page's members can be made an admin":   "Solo se puede hacer administrador a alguien que ya sea miembro de esta página",
	"Could not save the change. Try again in a moment":           "No se pudo guardar el cambio. Inténtalo de nuevo en un momento",
	"Could not reach the access provider. Try again in a moment": "No se pudo conectar con el proveedor de acceso. Inténtalo de nuevo en un momento",
	"That list cannot be edited from bifrost":                    "Esa lista no se puede editar desde bifrost",
	"A list cannot be left empty: add someone else first":        "Una lista no puede quedarse vacía: añade antes a otra persona",

	// Invite links
	"Open the link in a browser":              "Abre el enlace en un navegador",
	"This bifrost does not take invite links": "Este bifrost no admite enlaces de invitación",
	"Sign in first":                           "Inicia sesión primero",
	"Open the link from outside the house, where it asks you to sign in.":               "Abre el enlace desde fuera de casa, donde te pide iniciar sesión.",
	"Sign in with an email address":                                                     "Inicia sesión con una dirección de correo",
	"This invite link no longer works":                                                  "Este enlace de invitación ya no funciona",
	"Each link lets one person in and lasts a week. Ask whoever sent it for a new one.": "Cada enlace sirve para una persona y dura una semana. Pide uno nuevo a quien te lo envió.",
	"Could not let you in just now":                                                     "Ahora mismo no se te ha podido dar acceso",
	"Open the link again in a moment.":                                                  "Vuelve a abrir el enlace en un momento.",
	"The list it adds you to cannot be changed from bifrost any more.":                  "La lista a la que te añade ya no se puede cambiar desde bifrost.",
	"You're in": "Ya tienes acceso",
	"Cloudflare takes a few seconds to let you through: bifrost opens by itself in %d s.": "Cloudflare tarda unos segundos en dejarte pasar: bifrost se abrirá solo en %d s.",
	"Open bifrost": "Abrir bifrost",

	// Channels and sources
	"No such channel": "Ese canal no existe",
	"That is not one of this channel's sources":                          "Esa no es una de las fuentes de este canal",
	"The channel is not playing":                                         "El canal no se está reproduciendo",
	"Another switch is running, try again in a few seconds":              "Ya hay otro cambio en marcha, inténtalo de nuevo en unos segundos",
	"Source %d did not start":                                            "La fuente %d no arrancó",
	"The channel stopped playing":                                        "El canal dejó de reproducirse",
	"The channel is busy, try again in a few seconds":                    "El canal está ocupado, inténtalo de nuevo en unos segundos",
	"Checking sources needs ffmpeg":                                      "Comprobar fuentes necesita ffmpeg",
	"This channel's sources are being checked already":                   "Las fuentes de este canal ya se están comprobando",
	"This machine checks one channel at a time: %s is being checked":     "Esta máquina comprueba un canal cada vez: ahora está con %s",
	"This machine checks %d channels at a time, try again when one ends": "Esta máquina comprueba %d canales a la vez, inténtalo cuando termine uno",
	"This is the channel's last source":                                  "Es la última fuente del canal",
	"The channel is playing this source: pick another one first":         "El canal está reproduciendo esta fuente: elige antes otra",
	"The blocklist could not be saved":                                   "No se pudo guardar la lista de fuentes quitadas",
}
