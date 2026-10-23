package whatsapp

import (
	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"strings"
)

func interactiveProjection(m *waE2E.InteractiveMessage) *app.TextCardProjection {
	if m == nil || len(m.ProtoReflect().GetUnknown()) != 0 || m.BloksWidget != nil {
		return nil
	}
	switch v := m.InteractiveMessage.(type) {
	case nil:
	case *waE2E.InteractiveMessage_NativeFlowMessage_:
		if v == nil || v.NativeFlowMessage == nil {
			return nil
		}
	default:
		return nil
	}
	p := app.TextCardProjection{}
	if h := m.Header; h != nil {
		if h.Media != nil || h.GetHasMediaAttachment() || h.BloksWidget != nil || len(h.ProtoReflect().GetUnknown()) != 0 {
			return nil
		}
		p.Title = h.GetTitle()
		p.Subtitle = h.GetSubtitle()
	}
	if b := m.Body; b != nil {
		if len(b.ProtoReflect().GetUnknown()) != 0 {
			return nil
		}
		p.Body = b.GetText()
	}
	if f := m.Footer; f != nil {
		if f.Media != nil || f.GetHasMediaAttachment() || len(f.ProtoReflect().GetUnknown()) != 0 {
			return nil
		}
		p.Footer = f.GetText()
	}
	if !app.TextCardProjectionValid(p) {
		return nil
	}
	return &p
}
func cardFallback(p *app.TextCardProjection) string {
	for _, s := range append([]string{p.Body, p.Title, p.Subtitle, p.Footer}, p.Labels...) {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
func projectedInteractiveContent(m *waE2E.InteractiveMessage) extractedContent {
	p := interactiveProjection(m)
	if p == nil {
		return unsupportedContent()
	}
	return extractedContent{kind: "interactive", body: cardFallback(p), interactive: p}
}

func projectedTemplateContent(m *waE2E.TemplateMessage) extractedContent {
	if m == nil || len(m.ProtoReflect().GetUnknown()) != 0 {
		return unsupportedContent()
	}
	h := m.HydratedTemplate
	switch v := m.Format.(type) {
	case nil:
	case *waE2E.TemplateMessage_HydratedFourRowTemplate_:
		if v == nil || v.HydratedFourRowTemplate == nil || h != nil {
			return unsupportedContent()
		}
		h = v.HydratedFourRowTemplate
	case *waE2E.TemplateMessage_InteractiveMessageTemplate:
		if v == nil || h != nil {
			return unsupportedContent()
		}
		p := interactiveProjection(v.InteractiveMessageTemplate)
		if p == nil {
			return unsupportedContent()
		}
		return extractedContent{kind: "template", body: cardFallback(p), template: p}
	default:
		return unsupportedContent()
	}
	if h == nil || len(h.ProtoReflect().GetUnknown()) != 0 || len(h.HydratedButtons) > 4 {
		return unsupportedContent()
	}
	title, ok := h.Title.(*waE2E.TemplateMessage_HydratedFourRowTemplate_HydratedTitleText)
	if !ok || title == nil {
		return unsupportedContent()
	}
	p := app.TextCardProjection{Title: title.HydratedTitleText, Body: h.GetHydratedContentText(), Footer: h.GetHydratedFooterText()}
	for _, b := range h.HydratedButtons {
		if b == nil || len(b.ProtoReflect().GetUnknown()) != 0 {
			return unsupportedContent()
		}
		var label *string
		switch v := b.HydratedButton.(type) {
		case *waE2E.HydratedTemplateButton_QuickReplyButton:
			if v == nil || v.QuickReplyButton == nil || len(v.QuickReplyButton.ProtoReflect().GetUnknown()) != 0 {
				return unsupportedContent()
			}
			label = v.QuickReplyButton.DisplayText
		case *waE2E.HydratedTemplateButton_UrlButton:
			if v == nil || v.UrlButton == nil || len(v.UrlButton.ProtoReflect().GetUnknown()) != 0 {
				return unsupportedContent()
			}
			label = v.UrlButton.DisplayText
		case *waE2E.HydratedTemplateButton_CallButton:
			if v == nil || v.CallButton == nil || len(v.CallButton.ProtoReflect().GetUnknown()) != 0 {
				return unsupportedContent()
			}
			label = v.CallButton.DisplayText
		default:
			return unsupportedContent()
		}
		if label == nil {
			return unsupportedContent()
		}
		p.Labels = append(p.Labels, *label)
	}
	if !app.TextCardProjectionValid(p) {
		return unsupportedContent()
	}
	return extractedContent{kind: "template", body: cardFallback(&p), template: &p}
}
