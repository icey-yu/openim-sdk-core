// Copyright © 2023 OpenIM SDK. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package conversation_msg

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/openimsdk/openim-sdk-core/v3/pkg/common"
	"github.com/openimsdk/openim-sdk-core/v3/pkg/constant"
	"github.com/openimsdk/openim-sdk-core/v3/pkg/db/model_struct"
	"github.com/openimsdk/openim-sdk-core/v3/pkg/utils"
	"github.com/openimsdk/openim-sdk-core/v3/sdk_struct"

	"github.com/openimsdk/protocol/sdkws"
	"github.com/openimsdk/tools/errs"
	"github.com/openimsdk/tools/log"
)

const (
	syncWait = iota
	asyncNoWait
	asyncWait
)

// InitSyncProgress is initialize Sync when reinstall.
const InitSyncProgress = 10

func (c *Conversation) Work(c2v common.Cmd2Value) {
	log.ZDebug(c2v.Ctx, "NotificationCmd start", "cmd", c2v.Cmd, "value", c2v.Value)
	defer log.ZDebug(c2v.Ctx, "NotificationCmd end", "cmd", c2v.Cmd, "value", c2v.Value)
	switch c2v.Cmd {
	case constant.CmdNewMsgCome:
		c.doMsgNew(c2v)
	case constant.CmdUpdateConversation:
		c.doUpdateConversation(c2v)
	case constant.CmdUpdateMessage:
		c.doUpdateMessage(c2v)
	case constant.CmSyncReactionExtensions:
	case constant.CmdNotification:
		c.doNotificationManager(c2v)
	case constant.CmdSyncData:
		c.syncData(c2v)
	case constant.CmdSyncFlag:
		c.syncFlag(c2v)
	case constant.CmdMsgSyncInReinstall:
		c.doMsgSyncByReinstalled(c2v)
	}
}

func (c *Conversation) syncFlag(c2v common.Cmd2Value) {
	ctx := c2v.Ctx
	syncFlag := c2v.Value.(sdk_struct.CmdNewMsgComeToConversation).SyncFlag
	switch syncFlag {
	case constant.AppDataSyncStart:
		log.ZDebug(ctx, "AppDataSyncStart")
		c.startTime = time.Now()
		c.ConversationListener().OnSyncServerStart(true)
		c.ConversationListener().OnSyncServerProgress(1)
		asyncWaitFunctions := []func(c context.Context) error{
			c.group.SyncAllJoinedGroupsAndMembers,
			c.relation.IncrSyncFriends,
		}
		runSyncFunctions(ctx, asyncWaitFunctions, asyncWait)
		c.addInitProgress(InitSyncProgress * 4 / 10)              // add 40% of InitSyncProgress as progress
		c.ConversationListener().OnSyncServerProgress(c.progress) // notify server current Progress

		syncWaitFunctions := []func(c context.Context) error{
			c.IncrSyncConversations,
			c.SyncAllConversationHashReadSeqs,
		}
		runSyncFunctions(ctx, syncWaitFunctions, syncWait)
		log.ZWarn(ctx, "core data sync over", nil, "cost time", time.Since(c.startTime).Seconds())
		c.addInitProgress(InitSyncProgress * 6 / 10)              // add 60% of InitSyncProgress as progress
		c.ConversationListener().OnSyncServerProgress(c.progress) // notify server current Progress

		asyncNoWaitFunctions := []func(c context.Context) error{
			c.user.SyncLoginUserInfoWithoutNotice,
			c.relation.SyncAllBlackListWithoutNotice,
			c.relation.SyncAllFriendApplicationWithoutNotice,
			c.relation.SyncAllSelfFriendApplicationWithoutNotice,
			c.group.SyncAllAdminGroupApplicationWithoutNotice,
			c.group.SyncAllSelfGroupApplicationWithoutNotice,
			c.user.SyncAllCommandWithoutNotice,
		}
		runSyncFunctions(ctx, asyncNoWaitFunctions, asyncNoWait)

	case constant.AppDataSyncFinish:
		log.ZDebug(ctx, "AppDataSyncFinish", "time", time.Since(c.startTime).Milliseconds())
		c.progress = 100
		c.ConversationListener().OnSyncServerProgress(c.progress)
		c.ConversationListener().OnSyncServerFinish(true)
	case constant.MsgSyncBegin:
		log.ZDebug(ctx, "MsgSyncBegin")
		c.ConversationListener().OnSyncServerStart(false)
		c.syncData(c2v)
	case constant.MsgSyncFailed:
		c.ConversationListener().OnSyncServerFailed(false)
	case constant.MsgSyncEnd:
		log.ZDebug(ctx, "MsgSyncEnd", "time", time.Since(c.startTime).Milliseconds())
		c.ConversationListener().OnSyncServerFinish(false)
	}
}

func (c *Conversation) doNotificationManager(c2v common.Cmd2Value) {
	ctx := c2v.Ctx
	allMsg := c2v.Value.(sdk_struct.CmdNewMsgComeToConversation).Msgs

	for conversationID, msgs := range allMsg {
		log.ZDebug(ctx, "notification handling", "conversationID", conversationID, "msgs", msgs)
		if len(msgs.Msgs) != 0 {
			lastMsg := msgs.Msgs[len(msgs.Msgs)-1]
			log.ZDebug(ctx, "SetNotificationSeq", "conversationID", conversationID, "seq", lastMsg.Seq)
			if lastMsg.Seq != 0 {
				if err := c.db.SetNotificationSeq(ctx, conversationID, lastMsg.Seq); err != nil {
					log.ZError(ctx, "SetNotificationSeq err", err, "conversationID", conversationID, "lastMsg", lastMsg)
				}
			}
		}
		for _, msg := range msgs.Msgs {
			if msg.ContentType > constant.FriendNotificationBegin && msg.ContentType < constant.FriendNotificationEnd {
				c.relation.DoNotification(ctx, msg)
			} else if msg.ContentType > constant.UserNotificationBegin && msg.ContentType < constant.UserNotificationEnd {
				c.user.DoNotification(ctx, msg)
			} else if msg.ContentType > constant.GroupNotificationBegin && msg.ContentType < constant.GroupNotificationEnd {
				c.group.DoNotification(ctx, msg)
			} else {
				c.DoNotification(ctx, msg)
			}
		}
	}
}

func (c *Conversation) DoNotification(ctx context.Context, msg *sdkws.MsgData) {
	go func() {
		if err := c.doNotification(ctx, msg); err != nil {
			log.ZError(ctx, "DoGroupNotification failed", err)
		}
	}()
}

func (c *Conversation) doNotification(ctx context.Context, msg *sdkws.MsgData) error {
	switch msg.ContentType {
	case constant.ConversationChangeNotification:
		return c.DoConversationChangedNotification(ctx, msg)
	case constant.ConversationPrivateChatNotification: // 1701
		return c.DoConversationIsPrivateChangedNotification(ctx, msg)
	case constant.BusinessNotification:
		return c.doBusinessNotification(ctx, msg)
	case constant.RevokeNotification: // 2101
		return c.doRevokeMsg(ctx, msg)
	case constant.ClearConversationNotification: // 1703
		return c.doClearConversations(ctx, msg)
	case constant.DeleteMsgsNotification:
		return c.doDeleteMsgs(ctx, msg)
	case constant.HasReadReceipt: // 2200
		return c.doReadDrawing(ctx, msg)
	}
	return errs.New("unknown tips type", "contentType", msg.ContentType).Wrap()
}

func (c *Conversation) getConversationLatestMsgClientID(latestMsg string) string {
	msg := &sdk_struct.MsgStruct{}
	if err := json.Unmarshal([]byte(latestMsg), msg); err != nil {
		log.ZError(context.Background(), "getConversationLatestMsgClientID", err, "latestMsg", latestMsg)
	}
	return msg.ClientMsgID
}

func (c *Conversation) doUpdateConversation(c2v common.Cmd2Value) {
	ctx := c2v.Ctx
	node := c2v.Value.(common.UpdateConNode)
	switch node.Action {
	case constant.AddConOrUpLatMsg:
		var list []*model_struct.LocalConversation
		lc := node.Args.(model_struct.LocalConversation)
		oc, err := c.db.GetConversation(ctx, lc.ConversationID)
		if err == nil {
			// log.Info("this is old conversation", *oc)
			if lc.LatestMsgSendTime >= oc.LatestMsgSendTime || c.getConversationLatestMsgClientID(lc.LatestMsg) == c.getConversationLatestMsgClientID(oc.LatestMsg) { // The session update of asynchronous messages is subject to the latest sending time
				err := c.db.UpdateColumnsConversation(ctx, node.ConID, map[string]interface{}{"latest_msg_send_time": lc.LatestMsgSendTime, "latest_msg": lc.LatestMsg})
				if err != nil {
					log.ZError(ctx, "updateConversationLatestMsgModel", err, "conversationID", node.ConID)
				} else {
					oc.LatestMsgSendTime = lc.LatestMsgSendTime
					oc.LatestMsg = lc.LatestMsg
					list = append(list, oc)
					c.ConversationListener().OnConversationChanged(utils.StructToJsonString(list))
				}
			}
		} else {
			// log.Info("this is new conversation", lc)
			err4 := c.db.InsertConversation(ctx, &lc)
			if err4 != nil {
				// log.Error("internal", "insert new conversation err:", err4.Error())
			} else {
				list = append(list, &lc)
				c.ConversationListener().OnNewConversation(utils.StructToJsonString(list))
			}
		}

	case constant.UnreadCountSetZero:
		if err := c.db.UpdateColumnsConversation(ctx, node.ConID, map[string]interface{}{"unread_count": 0}); err != nil {
			log.ZError(ctx, "updateConversationUnreadCountModel err", err, "conversationID", node.ConID)
		} else {
			totalUnreadCount, err := c.db.GetTotalUnreadMsgCountDB(ctx)
			if err == nil {
				c.ConversationListener().OnTotalUnreadMessageCountChanged(totalUnreadCount)
			} else {
				log.ZError(ctx, "getTotalUnreadMsgCountDB err", err)
			}

		}
	case constant.IncrUnread:
		err := c.db.IncrConversationUnreadCount(ctx, node.ConID)
		if err != nil {
			// log.Error("internal", "incrConversationUnreadCount database err:", err.Error())
			return
		}
	case constant.TotalUnreadMessageChanged:
		totalUnreadCount, err := c.db.GetTotalUnreadMsgCountDB(ctx)
		if err != nil {
			// log.Error("internal", "TotalUnreadMessageChanged database err:", err.Error())
		} else {
			c.ConversationListener().OnTotalUnreadMessageCountChanged(totalUnreadCount)
		}
	case constant.UpdateConFaceUrlAndNickName:
		var lc model_struct.LocalConversation
		st := node.Args.(common.SourceIDAndSessionType)
		log.ZInfo(ctx, "UpdateConFaceUrlAndNickName", "st", st)
		switch st.SessionType {
		case constant.SingleChatType:
			lc.UserID = st.SourceID
			lc.ConversationID = c.getConversationIDBySessionType(st.SourceID, constant.SingleChatType)
			lc.ConversationType = constant.SingleChatType
		case constant.SuperGroupChatType:
			conversationID, conversationType, err := c.getConversationTypeByGroupID(ctx, st.SourceID)
			if err != nil {
				return
			}
			lc.GroupID = st.SourceID
			lc.ConversationID = conversationID
			lc.ConversationType = conversationType
		case constant.NotificationChatType:
			lc.UserID = st.SourceID
			lc.ConversationID = c.getConversationIDBySessionType(st.SourceID, constant.NotificationChatType)
			lc.ConversationType = constant.NotificationChatType
		default:
			log.ZError(ctx, "not support sessionType", nil, "sessionType", st.SessionType)
			return
		}
		lc.ShowName = st.Nickname
		lc.FaceURL = st.FaceURL
		err := c.db.UpdateConversation(ctx, &lc)
		if err != nil {
			// log.Error("internal", "setConversationFaceUrlAndNickName database err:", err.Error())
			return
		}
		c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{ConID: lc.ConversationID, Action: constant.ConChange, Args: []string{lc.ConversationID}}})

	case constant.UpdateLatestMessageChange:
		conversationID := node.ConID
		var latestMsg sdk_struct.MsgStruct
		l, err := c.db.GetConversation(ctx, conversationID)
		if err != nil {
			log.ZError(ctx, "getConversationLatestMsgModel err", err, "conversationID", conversationID)
		} else {
			err := json.Unmarshal([]byte(l.LatestMsg), &latestMsg)
			if err != nil {
				log.ZError(ctx, "latestMsg,Unmarshal err", err)
			} else {
				latestMsg.IsRead = true
				newLatestMessage := utils.StructToJsonString(latestMsg)
				err = c.db.UpdateColumnsConversation(ctx, node.ConID, map[string]interface{}{"latest_msg_send_time": latestMsg.SendTime, "latest_msg": newLatestMessage})
				if err != nil {
					log.ZError(ctx, "updateConversationLatestMsgModel err", err)
				}
			}
		}
	case constant.ConChange:
		conversationIDs := node.Args.([]string)
		conversations, err := c.db.GetMultipleConversationDB(ctx, conversationIDs)
		if err != nil {
			log.ZError(ctx, "getMultipleConversationModel err", err)
		} else {
			var newCList []*model_struct.LocalConversation
			for _, v := range conversations {
				if v.LatestMsgSendTime != 0 {
					newCList = append(newCList, v)
				}
			}
			c.ConversationListener().OnConversationChanged(utils.StructToJsonStringDefault(newCList))
		}
	case constant.NewCon:
		cidList := node.Args.([]string)
		cLists, err := c.db.GetMultipleConversationDB(ctx, cidList)
		if err != nil {
			// log.Error("internal", "getMultipleConversationModel err :", err.Error())
		} else {
			if cLists != nil {
				// log.Info("internal", "getMultipleConversationModel success :", cLists)
				c.ConversationListener().OnNewConversation(utils.StructToJsonString(cLists))
			}
		}
	case constant.ConChangeDirect:
		cidList := node.Args.(string)
		c.ConversationListener().OnConversationChanged(cidList)

	case constant.NewConDirect:
		cidList := node.Args.(string)
		// log.Debug("internal", "NewConversation", cidList)
		c.ConversationListener().OnNewConversation(cidList)

	case constant.ConversationLatestMsgHasRead:
		hasReadMsgList := node.Args.(map[string][]string)
		var result []*model_struct.LocalConversation
		var latestMsg sdk_struct.MsgStruct
		var lc model_struct.LocalConversation
		for conversationID, msgIDList := range hasReadMsgList {
			LocalConversation, err := c.db.GetConversation(ctx, conversationID)
			if err != nil {
				// log.Error("internal", "get conversation err", err.Error(), conversationID)
				continue
			}
			err = utils.JsonStringToStruct(LocalConversation.LatestMsg, &latestMsg)
			if err != nil {
				// log.Error("internal", "JsonStringToStruct err", err.Error(), conversationID)
				continue
			}
			if utils.IsContain(latestMsg.ClientMsgID, msgIDList) {
				latestMsg.IsRead = true
				lc.ConversationID = conversationID
				lc.LatestMsg = utils.StructToJsonString(latestMsg)
				LocalConversation.LatestMsg = utils.StructToJsonString(latestMsg)
				err := c.db.UpdateConversation(ctx, &lc)
				if err != nil {
					// log.Error("internal", "UpdateConversation database err:", err.Error())
					continue
				} else {
					result = append(result, LocalConversation)
				}
			}
		}
		if result != nil {
			// log.Info("internal", "getMultipleConversationModel success :", result)
			c.ConversationListener().OnNewConversation(utils.StructToJsonString(result))
		}
	case constant.SyncConversation:

	}
}

func (c *Conversation) syncData(c2v common.Cmd2Value) {
	c.conversationSyncMutex.Lock()
	defer c.conversationSyncMutex.Unlock()

	ctx := c2v.Ctx
	c.startTime = time.Now()
	//clear SubscriptionStatusMap
	//c.user.OnlineStatusCache.DeleteAll()

	// Synchronous sync functions
	syncFuncs := []func(c context.Context) error{
		c.SyncAllConversationHashReadSeqs,
	}

	runSyncFunctions(ctx, syncFuncs, syncWait)

	// Asynchronous sync functions
	asyncFuncs := []func(c context.Context) error{
		c.user.SyncLoginUserInfo,
		c.relation.SyncAllBlackList,
		c.relation.SyncAllFriendApplication,
		c.relation.SyncAllSelfFriendApplication,
		c.group.SyncAllAdminGroupApplication,
		c.group.SyncAllSelfGroupApplication,
		c.user.SyncAllCommand,
		c.group.SyncAllJoinedGroupsAndMembers,
		c.relation.IncrSyncFriends,
		c.IncrSyncConversations,
	}

	runSyncFunctions(ctx, asyncFuncs, asyncNoWait)
}

func runSyncFunctions(ctx context.Context, funcs []func(c context.Context) error, mode int) {
	var wg sync.WaitGroup

	for _, fn := range funcs {
		switch mode {
		case asyncWait:
			wg.Add(1)
			go executeSyncFunction(ctx, fn, &wg)
		case asyncNoWait:
			go executeSyncFunction(ctx, fn, nil)
		case syncWait:
			executeSyncFunction(ctx, fn, nil)
		}
	}

	if mode == asyncWait {
		wg.Wait()
	}
}

func executeSyncFunction(ctx context.Context, fn func(c context.Context) error, wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}

	funcName := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
	startTime := time.Now()
	err := fn(ctx)
	duration := time.Since(startTime)
	if err != nil {
		log.ZWarn(ctx, fmt.Sprintf("%s sync error", funcName), err, "duration", duration.Seconds())
	} else {
		log.ZDebug(ctx, fmt.Sprintf("%s completed successfully", funcName), "duration", duration.Seconds())
	}
}

func (c *Conversation) doUpdateMessage(c2v common.Cmd2Value) {
	node := c2v.Value.(common.UpdateMessageNode)
	ctx := c2v.Ctx
	switch node.Action {
	case constant.UpdateMsgFaceUrlAndNickName:
		args := node.Args.(common.UpdateMessageInfo)
		switch args.SessionType {
		case constant.SingleChatType:
			if args.UserID == c.loginUserID {
				conversationIDList, err := c.db.GetAllSingleConversationIDList(ctx)
				if err != nil {
					log.ZError(ctx, "GetAllSingleConversationIDList err", err)
					return
				} else {
					log.ZDebug(ctx, "get single conversationID list", "conversationIDList", conversationIDList)
					for _, conversationID := range conversationIDList {
						err := c.db.UpdateMsgSenderFaceURLAndSenderNickname(ctx, conversationID, args.UserID, args.FaceURL, args.Nickname)
						if err != nil {
							log.ZError(ctx, "UpdateMsgSenderFaceURLAndSenderNickname err", err)
							continue
						}
					}

				}
			} else {
				conversationID := c.getConversationIDBySessionType(args.UserID, constant.SingleChatType)
				err := c.db.UpdateMsgSenderFaceURLAndSenderNickname(ctx, conversationID, args.UserID, args.FaceURL, args.Nickname)
				if err != nil {
					log.ZError(ctx, "UpdateMsgSenderFaceURLAndSenderNickname err", err)
				}

			}
		case constant.SuperGroupChatType:
			conversationID := c.getConversationIDBySessionType(args.GroupID, constant.SuperGroupChatType)
			err := c.db.UpdateMsgSenderFaceURLAndSenderNickname(ctx, conversationID, args.UserID, args.FaceURL, args.Nickname)
			if err != nil {
				log.ZError(ctx, "UpdateMsgSenderFaceURLAndSenderNickname err", err)
			}
		case constant.NotificationChatType:
			conversationID := c.getConversationIDBySessionType(args.UserID, constant.NotificationChatType)
			err := c.db.UpdateMsgSenderFaceURLAndSenderNickname(ctx, conversationID, args.UserID, args.FaceURL, args.Nickname)
			if err != nil {
				log.ZError(ctx, "UpdateMsgSenderFaceURLAndSenderNickname err", err)
			}
		default:
			log.ZError(ctx, "not support sessionType", nil, "args", args)
			return
		}
	}

}

func (c *Conversation) DoConversationChangedNotification(ctx context.Context, msg *sdkws.MsgData) error {
	c.conversationSyncMutex.Lock()
	defer c.conversationSyncMutex.Unlock()

	//var notification sdkws.ConversationChangedNotification
	tips := &sdkws.ConversationUpdateTips{}
	if err := utils.UnmarshalNotificationElem(msg.Content, tips); err != nil {
		log.ZWarn(ctx, "UnmarshalNotificationElem err", err, "msg", msg)
		return err
	}

	err := c.IncrSyncConversations(ctx)
	if err != nil {
		log.ZWarn(ctx, "IncrSyncConversations err", err)
		return err
	}
	return nil
}

func (c *Conversation) DoConversationIsPrivateChangedNotification(ctx context.Context, msg *sdkws.MsgData) error {
	c.conversationSyncMutex.Lock()
	defer c.conversationSyncMutex.Unlock()

	tips := &sdkws.ConversationSetPrivateTips{}
	if err := utils.UnmarshalNotificationElem(msg.Content, tips); err != nil {
		log.ZWarn(ctx, "UnmarshalNotificationElem err", err, "msg", msg)
		return err
	}

	err := c.IncrSyncConversations(ctx)
	if err != nil {
		log.ZWarn(ctx, "IncrSyncConversations err", err)
		return err
	}
	return nil
}

func (c *Conversation) doBusinessNotification(ctx context.Context, msg *sdkws.MsgData) error {
	var n sdk_struct.NotificationElem
	err := utils.JsonStringToStruct(string(msg.Content), &n)
	if err != nil {
		log.ZError(ctx, "unmarshal failed", err, "msg", msg)
		return err

	}
	c.businessListener().OnRecvCustomBusinessMessage(n.Detail)
	return nil
}