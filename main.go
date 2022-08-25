package main

import (
	"context"
	"fmt"
	"kubinka/config"
	"kubinka/errlist"
	"kubinka/service"
	"kubinka/strg"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

func getLogFile(fileName string) *os.File {
	f, err := os.OpenFile(fileName, os.O_APPEND|os.O_WRONLY, os.ModeAppend)
	if err != nil {
		_, err := os.Create(fileName)
		if err != nil {
			log.Fatalln("Failed to create or open file for logging.")
		}
		f, err := os.OpenFile(fileName, os.O_APPEND|os.O_WRONLY, os.ModeAppend)
		if err != nil {
			log.Fatalln("Failed to open just created file.")
		}
		return f
	}
	return f
}

func newDiscordSession(
	ctx context.Context,
	cancel context.CancelFunc,
	token string,
	c *strg.BoltConn,
) (*discordgo.Session, *service.MasterHandler) {
	discord, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal("Could not create session.\n\n\n")
	}
	discord.SyncEvents = false
	masterHandler := service.NewMasterHandler(ctx, cancel, c)
	discord.AddHandler(masterHandler.Handle)

	err = discord.Open()
	if err != nil {
		log.Fatal("Could not open connection.\n\n\n")
	}

	return discord, masterHandler
}

func createCommands(ds *discordgo.Session, appId string, guildId string) {
	var err error
	for i, cmd := range service.CmdDef {
		service.CmdDef[i], err = ds.ApplicationCommandCreate(
			appId,
			guildId,
			cmd,
		)
		if err != nil {
			if i > 0 {
				deleteCommands(ds, config.BOT_GUILD_ID)
			}
			log.Fatalf("Failed to create command %s:\n %s\n\n\n", cmd.Name, err)
		}
	}
}

func deleteCommands(ds *discordgo.Session, guildId string) {
	for _, cmd := range service.CmdDef {
		err := ds.ApplicationCommandDelete(
			ds.State.User.ID,
			guildId,
			cmd.ID,
		)
		if err != nil {
			log.Fatalf("Could not delete %q command: %v\n\n\n", cmd.Name, err)
		}
	}
}

// ds.GuildRoleDelete(guildId, roleId) to rollback
func createRole(ds *discordgo.Session, name, guildId string, color int) (roleId string, err error) {
	st, err := ds.GuildRoleCreate(guildId)
	if err != nil {
		return "", err
	}

	if _, err := ds.GuildRoleEdit(guildId, st.ID, name, color, true, st.Permissions, true); err != nil {
		return st.ID, err
	}

	return st.ID, nil
}

func reissueRoles(ds *discordgo.Session, db *strg.BoltConn, guildId, roleId string) error {
	idsWithRoles := db.GetPlayerIDs()
	for _, id := range idsWithRoles {
		err := ds.GuildMemberRoleAdd(guildId, id, roleId)
		if err != nil {
			return errlist.New(err).Set("event", errlist.StartupRoleReissue).Set("session", id)
		}
	}

	return nil
}

func main() {
	logFile := getLogFile(config.LOG_FILE_NAME)
	defer logFile.Close()
	log.SetFlags(log.Flags() &^ (log.Ldate | log.Ltime))
	log.SetOutput(logFile)
	log.Print(errlist.New(nil).Set("event", "SESSION STARTUP"))
	shutdownLogRec := errlist.New(nil).Set("event", "SESSION SHUTDOWN")

	db, err := strg.Connect(config.DB_NAME, config.DB_PLAYERS_BUCKET_NAME)
	if err != nil {
		log.Panicf("failed to connect to db: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())

	ds, masterHandler := newDiscordSession(ctx, cancel, config.BOT_TOKEN, db)
	defer masterHandler.Cancel()
	defer masterHandler.HaltUntilAllDone()
	defer ds.Close()

	createCommands(ds, config.BOT_APP_ID, config.BOT_GUILD_ID)
	defer deleteCommands(ds, config.BOT_GUILD_ID) // Removing commands on bot shutdown

	config.BOT_ROLE_ID, err = createRole(ds, "Waiting deploy", config.BOT_GUILD_ID, 307015)
	if err != nil {
		shutdownLogRec.Wrap(
			errlist.New(fmt.Errorf("failed to create role: %w", err)).
				Set("event", "startup_role_create"))
	}
	defer func() {
		if err := ds.GuildRoleDelete(config.BOT_GUILD_ID, config.BOT_ROLE_ID); err != nil {
			shutdownLogRec.Wrap(
				errlist.New(fmt.Errorf("failed to delete role")).
					Set("event", "shutdown_role_delete"))
		}
	}()

	reissueRoles(ds, db, config.BOT_GUILD_ID, config.BOT_ROLE_ID)

	go func() {
		err := db.WatchExpirations(ctx, ds)
		shutdownLogRec.Wrap(err)
		masterHandler.Cancel()
	}()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGTERM, syscall.SIGINT)
halt:
	for {
		select {
		case <-interrupt:
			log.Print(shutdownLogRec.Set("cause", "execution stopped by user"))
			masterHandler.Cancel()
			break halt
		case <-masterHandler.Ctx.Done():
			log.Print(shutdownLogRec.Set("cause", ctx.Err().Error()))
			break halt
		default:
			time.Sleep(time.Millisecond * 100)
		}
	}
}
